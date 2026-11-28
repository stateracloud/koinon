// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The LimitRange and the ResourceQuota of the Substrate worker namespace (issue #1161), as
// `helm template` renders them, against a real API server (envtest): a pod with no resources gets the
// default requests and limits, and a pod that goes over the quota is refused. The quota is rendered
// small here (two pods, one CPU of requests) so that the specs reach it fast. envtest runs no
// controller manager, so nobody computes the quota status: the spec sets status.hard itself, as the
// quota controller does, and the API server then counts every admitted pod. The chart also renders a
// ValidatingAdmissionPolicy that lets only the installer write the two objects: without it, any identity
// that can delete the quota removes the cap.
var _ = Describe("Worker namespace limits", Label("escape", "admission", "substrate"), Ordered, func() {
	var (
		cs *kubernetes.Clientset
		as = map[string]*kubernetes.Clientset{}
	)
	const (
		ns = "limits-workers"
		// What kubeadm and kind give their admin: not system:masters, but an installer group.
		kubeadmAdmin = "kubernetes-admin"
		reconciler   = "system:serviceaccount:koinon:koinon-reconciler"
		gcUser       = "system:serviceaccount:kube-system:generic-garbage-collector"
		nsController = "system:serviceaccount:kube-system:namespace-controller"
	)

	pod := func(name string, res corev1.ResourceRequirements) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "worker", Image: workerDigest, Resources: res,
			}}},
		}
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		helm, err := exec.LookPath(envOr("HELM", "helm"))
		Expect(err).NotTo(HaveOccurred(), "helm renders the chart's limits")
		var out, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, helm, "template", "koinon", "../../charts/koinon", "--namespace", "koinon",
			"--set", "execution.driver=substrate",
			"--set", "execution.substrate.controlAddress=api.ate-system.svc:443",
			"--set", "execution.substrate.routerAddress=atenet-router.ate-system.svc:443",
			"--set", "execution.substrate.tokenAudience=ateapi",
			"--set", "execution.substrate.allowUnhardened=true",
			"--set", "execution.substrate.defaultTier=dev",
			"--set", "execution.substrate.workerNetworkPolicy.enabled=false",
			"--set", "execution.substrate.workerNamespace="+ns,
			"--set-string", "execution.substrate.workerLimits.quota.hard.pods=2",
			"--set-string", "execution.substrate.workerLimits.quota.hard.requests\\.cpu=1",
			"--set", "sandboxObserver.enabled=false", "--set", "sandboxNetworkPolicy.enabled=false", "--set", "sandboxLimitRange.enabled=false",
			"--show-only", "templates/substrate-workerlimits.yaml")
		cmd.Stdout, cmd.Stderr = &out, &stderr
		Expect(cmd.Run()).To(Succeed(), stderr.String())

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		cs = kubernetes.NewForConfigOrDie(cfg)
		_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		var (
			quota   *corev1.ResourceQuota
			guard   *admissionregistrationv1.ValidatingAdmissionPolicy
			binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
		)
		for _, doc := range strings.Split(out.String(), "\n---") {
			var meta metav1.TypeMeta
			Expect(yaml.Unmarshal([]byte(doc), &meta)).To(Succeed())
			switch meta.Kind {
			case "LimitRange":
				lr := &corev1.LimitRange{}
				Expect(yaml.UnmarshalStrict([]byte(doc), lr)).To(Succeed())
				_, err = cs.CoreV1().LimitRanges(ns).Create(ctx, lr, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			case "ResourceQuota":
				quota = &corev1.ResourceQuota{}
				Expect(yaml.UnmarshalStrict([]byte(doc), quota)).To(Succeed())
				quota, err = cs.CoreV1().ResourceQuotas(ns).Create(ctx, quota, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			case "ValidatingAdmissionPolicy":
				guard = &admissionregistrationv1.ValidatingAdmissionPolicy{}
				Expect(yaml.UnmarshalStrict([]byte(doc), guard)).To(Succeed())
			case "ValidatingAdmissionPolicyBinding":
				binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
				Expect(yaml.UnmarshalStrict([]byte(doc), binding)).To(Succeed())
			case "":
			default:
				Fail("unexpected " + meta.Kind + " in the worker limits template")
			}
		}
		Expect(quota).NotTo(BeNil(), "the chart renders a worker ResourceQuota")
		// What the quota controller does: status.hard and a zero usage. Without it the API server skips the quota.
		quota.Status.Hard = quota.Spec.Hard
		quota.Status.Used = corev1.ResourceList{}
		for name := range quota.Spec.Hard {
			quota.Status.Used[name] = resource.MustParse("0")
		}
		_, err = cs.CoreV1().ResourceQuotas(ns).UpdateStatus(ctx, quota, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The write guard. Every other identity gets full RBAC rights, so that only admission can say no.
		Expect(guard).NotTo(BeNil(), "the chart renders the write guard of the worker limits")
		Expect(binding).NotTo(BeNil(), "the chart renders the binding of the write guard")
		as["admin"] = cs
		for _, user := range []string{kubeadmAdmin, workerController, reconciler, gcUser, nsController} {
			_, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-limits-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			groups := []string{"system:authenticated"}
			if parts := strings.Split(user, ":"); len(parts) == 4 && parts[1] == "serviceaccount" {
				groups = append(groups, "system:serviceaccounts", "system:serviceaccounts:"+parts[2])
			}
			if user == kubeadmAdmin {
				groups = []string{"kubeadm:cluster-admins", "system:authenticated"}
			}
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
			as[user] = kubernetes.NewForConfigOrDie(c)
		}
		_, err = cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, guard, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("waiting until the write guard is enforced")
		Eventually(func() error {
			return as[reconciler].CoreV1().ResourceQuotas(ns).Delete(ctx, quota.Name, metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
		}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("gives a pod without resources the default requests and limits", func(ctx SpecContext) {
		var created *corev1.Pod
		// The LimitRanger and quota plugins read their objects from a cache: wait until it has them.
		Eventually(func() error {
			var err error
			created, err = cs.CoreV1().Pods(ns).Create(ctx, pod("unsized", corev1.ResourceRequirements{}), metav1.CreateOptions{})
			return err
		}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(200 * time.Millisecond).Should(Succeed())
		res := created.Spec.Containers[0].Resources
		Expect(res.Requests.Cpu().String()).To(Equal("500m"))
		Expect(res.Requests.Memory().String()).To(Equal("1Gi"))
		Expect(res.Limits.Cpu().String()).To(Equal("8"))
		Expect(res.Limits.Memory().String()).To(Equal("16Gi"))
	})

	It("refuses a pod that goes over the CPU quota", func(ctx SpecContext) {
		// Requests of 2 CPU against a quota of 1.
		_, err := cs.CoreV1().Pods(ns).Create(ctx, pod("big", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		}), metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want a quota refusal, got %v", err)
		Expect(err.Error()).To(ContainSubstring("exceeded quota"))
		Expect(err.Error()).To(ContainSubstring("requests.cpu"))
	})

	It("refuses a pod over the pod count", func(ctx SpecContext) {
		small := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		}
		// "unsized" is the first pod (500m of the 1 CPU). The second fits. The third is over the count of two.
		_, err := cs.CoreV1().Pods(ns).Create(ctx, pod("second", small), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns).Create(ctx, pod("third", small), metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want a quota refusal, got %v", err)
		Expect(err.Error()).To(ContainSubstring("exceeded quota"))
		Expect(err.Error()).To(ContainSubstring("pods"))
	})

	It("lets only the installer write the LimitRange and the ResourceQuota", func(ctx SpecContext) {
		dry := []string{metav1.DryRunAll}
		denied := func(err error, what string) {
			GinkgoHelper()
			Expect(err).To(HaveOccurred(), "%s: admitted, want a denial by the write guard", what)
			Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"), what)
			Expect(err.Error()).To(ContainSubstring("only the installer may"), what)
		}
		current := func() (*corev1.LimitRange, *corev1.ResourceQuota) {
			lrs, err := cs.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(lrs.Items).To(HaveLen(1))
			rqs, err := cs.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rqs.Items).To(HaveLen(1))
			return &lrs.Items[0], &rqs.Items[0]
		}
		lr, rq := current()
		newLR := func() *corev1.LimitRange {
			return &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "extra"}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")},
			}}}}
		}
		newRQ := func() *corev1.ResourceQuota {
			return &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "extra"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1000")}}}
		}
		raised := func() (*corev1.LimitRange, *corev1.ResourceQuota) {
			l, q := lr.DeepCopy(), rq.DeepCopy()
			l.Spec.Limits[0].Default[corev1.ResourceCPU] = resource.MustParse("64")
			q.Spec.Hard[corev1.ResourcePods] = resource.MustParse("1000")
			return l, q
		}
		relabelled := func() (*corev1.LimitRange, *corev1.ResourceQuota) {
			l, q := lr.DeepCopy(), rq.DeepCopy()
			l.Labels, q.Labels = map[string]string{"gc": "mark"}, map[string]string{"gc": "mark"}
			return l, q
		}

		// The installer (system:masters, or kind's and kubeadm's admin group) writes and deletes both.
		for _, user := range []string{"admin", kubeadmAdmin} {
			_, err := as[user].CoreV1().LimitRanges(ns).Create(ctx, newLR(), metav1.CreateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			_, err = as[user].CoreV1().ResourceQuotas(ns).Create(ctx, newRQ(), metav1.CreateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			l, q := raised()
			_, err = as[user].CoreV1().LimitRanges(ns).Update(ctx, l, metav1.UpdateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			_, err = as[user].CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			Expect(as[user].CoreV1().LimitRanges(ns).Delete(ctx, lr.Name, metav1.DeleteOptions{DryRun: dry})).To(Succeed(), user)
			Expect(as[user].CoreV1().ResourceQuotas(ns).Delete(ctx, rq.Name, metav1.DeleteOptions{DryRun: dry})).To(Succeed(), user)
		}

		// Any other identity, the Substrate controller included, may not create, raise or delete them.
		for _, user := range []string{workerController, reconciler} {
			_, err := as[user].CoreV1().LimitRanges(ns).Create(ctx, newLR(), metav1.CreateOptions{DryRun: dry})
			denied(err, user+" creates a LimitRange")
			_, err = as[user].CoreV1().ResourceQuotas(ns).Create(ctx, newRQ(), metav1.CreateOptions{DryRun: dry})
			denied(err, user+" creates a ResourceQuota")
			l, q := raised()
			_, err = as[user].CoreV1().LimitRanges(ns).Update(ctx, l, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" raises the LimitRange")
			_, err = as[user].CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" raises the ResourceQuota")
			l, q = relabelled()
			_, err = as[user].CoreV1().LimitRanges(ns).Update(ctx, l, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" relabels the LimitRange")
			_, err = as[user].CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" relabels the ResourceQuota")
			denied(as[user].CoreV1().LimitRanges(ns).Delete(ctx, lr.Name, metav1.DeleteOptions{DryRun: dry}), user+" deletes the LimitRange")
			denied(as[user].CoreV1().ResourceQuotas(ns).Delete(ctx, rq.Name, metav1.DeleteOptions{DryRun: dry}), user+" deletes the ResourceQuota")
			// A collection delete is admitted item by item, as a DELETE of each object.
			denied(as[user].CoreV1().ResourceQuotas(ns).DeleteCollection(ctx, metav1.DeleteOptions{DryRun: dry}, metav1.ListOptions{}), user+" deletes all ResourceQuotas")
			denied(as[user].CoreV1().LimitRanges(ns).DeleteCollection(ctx, metav1.DeleteOptions{DryRun: dry}, metav1.ListOptions{}), user+" deletes all LimitRanges")
		}

		// Kubernetes' own controllers delete the objects with the namespace and may change metadata, never the spec.
		for _, user := range []string{gcUser, nsController} {
			Expect(as[user].CoreV1().LimitRanges(ns).Delete(ctx, lr.Name, metav1.DeleteOptions{DryRun: dry})).To(Succeed(), user)
			Expect(as[user].CoreV1().ResourceQuotas(ns).Delete(ctx, rq.Name, metav1.DeleteOptions{DryRun: dry})).To(Succeed(), user)
			l, q := relabelled()
			_, err := as[user].CoreV1().LimitRanges(ns).Update(ctx, l, metav1.UpdateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			_, err = as[user].CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{DryRun: dry})
			Expect(err).NotTo(HaveOccurred(), user)
			l, q = raised()
			_, err = as[user].CoreV1().LimitRanges(ns).Update(ctx, l, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" raises the LimitRange")
			_, err = as[user].CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{DryRun: dry})
			denied(err, user+" raises the ResourceQuota")
			_, err = as[user].CoreV1().ResourceQuotas(ns).Create(ctx, newRQ(), metav1.CreateOptions{DryRun: dry})
			denied(err, user+" creates a ResourceQuota")
			_, err = as[user].CoreV1().LimitRanges(ns).Create(ctx, newLR(), metav1.CreateOptions{DryRun: dry})
			denied(err, user+" creates a LimitRange")
		}
		// The namespace controller empties a namespace with a collection delete.
		Expect(as[nsController].CoreV1().ResourceQuotas(ns).DeleteCollection(ctx, metav1.DeleteOptions{DryRun: dry}, metav1.ListOptions{})).To(Succeed())
		Expect(as[nsController].CoreV1().LimitRanges(ns).DeleteCollection(ctx, metav1.DeleteOptions{DryRun: dry}, metav1.ListOptions{})).To(Succeed())

		// The quota status is a subresource: the quota controller still counts pods.
		q := rq.DeepCopy()
		q.Status.Used = corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1")}
		for name := range rq.Spec.Hard {
			if name != corev1.ResourcePods {
				q.Status.Used[name] = rq.Status.Used[name]
			}
		}
		_, err := as[reconciler].CoreV1().ResourceQuotas(ns).UpdateStatus(ctx, q, metav1.UpdateOptions{DryRun: dry})
		Expect(err).NotTo(HaveOccurred(), "the guard must not match resourcequotas/status")

		// The real objects are unchanged.
		l2, q2 := current()
		Expect(l2.Spec).To(Equal(lr.Spec))
		Expect(q2.Spec).To(Equal(rq.Spec))
	})
})
