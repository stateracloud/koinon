// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The chart template that renders the worker-namespace policies of the Substrate driver
// (ADR 0120 section 6, issue #1089). Until #1089 adds it, the specs below skip, and fail under
// KOINON_REQUIRE=substrate-policy.
const workerPolicyTemplate = "templates/substrate-policy.yaml"

// The worker pods' namespace and the identity that may create them. The defaults are Substrate's
// own; set the variables when the chart names them otherwise.
var (
	workerNamespace  = envOr("KOINON_ESCAPE_WORKER_NAMESPACE", "ate-system")
	workerController = envOr("KOINON_ESCAPE_WORKER_CONTROLLER", "system:serviceaccount:ate-system:ate-controller")
	// A hostPath of the documented set (ADR 0120 section 6.3): a worker needs the KVM device.
	workerHostPathOK = envOr("KOINON_ESCAPE_WORKER_HOSTPATH", "/dev/kvm")
)

// The digest of an image that exists only in these specs.
const workerDigest = "registry.invalid/koinon-escape/worker@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// The worker-namespace ValidatingAdmissionPolicies of the Substrate driver (ADR 0120 section 6),
// as `helm template` renders them, against a real API server (envtest): worker pods only from
// the controller identity, image digests pinned, no hostNetwork, no hostPath outside the documented
// set, no ephemeral containers and no exec into a worker. Pod Security admission is not enforced in
// envtest, so every denial is the policy's. These specs do not depend on the driver, but on the chart:
// they skip while the chart renders no worker policy.
var _ = Describe("Worker pod admission policy", Label("escape", "admission", "substrate"), Ordered, ContinueOnFailure, func() {
	var (
		admin  *kubernetes.Clientset
		as     map[string]*kubernetes.Clientset
		dryRun = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	)
	const (
		other    = "system:serviceaccount:koinon:koinon-reconciler"
		existing = "existing-worker"
	)

	worker := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: workerDigest}},
				Volumes: []corev1.Volume{{Name: "kvm", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: workerHostPathOK},
				}}},
			},
		}
		for _, m := range mutate {
			m(p)
		}
		return p
	}
	create := func(ctx context.Context, user, namespace string, p *corev1.Pod) error {
		_, err := as[user].CoreV1().Pods(namespace).Create(ctx, p, dryRun)
		return err
	}
	denied := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial by a ValidatingAdmissionPolicy")
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderWorkerPolicy(ctx)

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		for _, user := range []string{workerController, other} {
			// Full RBAC rights, so that only admission can say no.
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-worker-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			groups := []string{"system:serviceaccounts", "system:authenticated"}
			if parts := strings.Split(user, ":"); len(parts) == 4 && parts[1] == "serviceaccount" {
				// A real token also carries its namespace's group.
				groups = append(groups, "system:serviceaccounts:"+parts[2])
			}
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
			as[user] = kubernetes.NewForConfigOrDie(c)
		}
		_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: workerNamespace, Labels: map[string]string{"kubernetes.io/metadata.name": workerNamespace},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.CoreV1().ServiceAccounts(workerNamespace).Create(ctx,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The pod the update and connect cases act on exists before the policies do.
		_, err = admin.CoreV1().Pods(workerNamespace).Create(ctx, worker(existing), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		for _, p := range policies {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, p, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, b := range bindings {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, b, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("waiting until the policies are enforced")
		// A pod that breaks the hostNetwork and image rules, so that the wait does not depend on one of them.
		Eventually(func() error {
			return create(ctx, "admin", workerNamespace, worker("probe", func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/worker:latest"
			}))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("admits a compliant worker pod from the controller identity (control)", func(ctx SpecContext) {
		Expect(create(ctx, workerController, workerNamespace, worker("ok"))).To(Succeed())
	})

	It("denies a worker pod from any other identity", func(ctx SpecContext) {
		for _, user := range []string{"admin", other} {
			denied(create(ctx, user, workerNamespace, worker("by-"+strings.ToLower(strings.NewReplacer(":", "-", "_", "-").Replace(user)))))
		}
	})

	It("denies an image that is not pinned by digest", func(ctx SpecContext) {
		for _, image := range []string{"registry.invalid/koinon-escape/worker:v1", "registry.invalid/koinon-escape/worker:latest", "registry.invalid/koinon-escape/worker"} {
			denied(create(ctx, workerController, workerNamespace, worker("tag", func(p *corev1.Pod) { p.Spec.Containers[0].Image = image })))
		}
		denied(create(ctx, workerController, workerNamespace, worker("init-tag", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "registry.invalid/koinon-escape/init:v1"}}
		})))
	})

	It("denies hostNetwork and a hostPath outside the documented set", func(ctx SpecContext) {
		denied(create(ctx, workerController, workerNamespace, worker("host-network", func(p *corev1.Pod) { p.Spec.HostNetwork = true })))
		for _, path := range []string{"/", "/etc", "/var/run/docker.sock", "/var/lib/kubelet", workerHostPathOK + "x"} {
			denied(create(ctx, workerController, workerNamespace, worker("host-path", func(p *corev1.Pod) {
				p.Spec.Volumes[0].HostPath.Path = path
			})))
			// The documented volume stays, so a policy that reads only the first volume fails here.
			denied(create(ctx, workerController, workerNamespace, worker("host-path-second", func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "extra", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: path},
				}})
			})))
		}
	})

	It("denies ephemeral containers in a worker pod, the controller's included", func(ctx SpecContext) {
		for _, user := range []string{workerController, other, "admin"} {
			p, err := admin.CoreV1().Pods(workerNamespace).Get(ctx, existing, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: workerDigest},
			}}
			_, err = as[user].CoreV1().Pods(workerNamespace).UpdateEphemeralContainers(ctx, existing, p, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			denied(err)
		}
	})

	It("denies exec into a worker pod, for every identity", func(ctx SpecContext) {
		// Admission runs before the API server opens the connection, so this needs no kubelet.
		for _, user := range []string{"admin", workerController, other} {
			req := as[user].CoreV1().RESTClient().Post().Param("command", "id").Param("stdout", "true")
			denied(req.Namespace(workerNamespace).Resource("pods").Name(existing).SubResource("exec").Do(ctx).Error())
		}
	})
})

// renderWorkerPolicy renders the chart's worker policies with the Substrate driver, or skips the
// calling container while the chart has no such template. A skip becomes a failure under
// KOINON_REQUIRE=substrate-policy.
func renderWorkerPolicy(ctx context.Context) ([]*admissionv1.ValidatingAdmissionPolicy, []*admissionv1.ValidatingAdmissionPolicyBinding) {
	GinkgoHelper()
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	Expect(err).NotTo(HaveOccurred(), "helm renders the chart's policies")
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helm, "template", "koinon", "../../charts/koinon", "--namespace", "koinon",
		"--set", "execution.driver=substrate",
		"--set", "execution.substrate.controlAddress=api.ate-system.svc:443",
		"--set", "execution.substrate.routerAddress=atenet-router.ate-system.svc:80",
		"--set", "execution.substrate.tokenAudience=ateapi",
		"--set", "execution.substrate.allowUnhardened=true",
		"--set", "sandboxObserver.enabled=false", "--set", "sandboxNetworkPolicy.enabled=false", "--set", "sandboxLimitRange.enabled=false",
		"--show-only", workerPolicyTemplate)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "could not find template") {
			if testenv.Required(os.Getenv(testenv.RequireVar), "substrate-policy") {
				Fail("the chart renders no worker policy (" + workerPolicyTemplate + ", issue #1089), and KOINON_REQUIRE has substrate-policy")
			}
			Skip(testenv.Message("substrate-policy", err) + ": the chart renders no worker policy yet (issue #1089)")
		}
		Fail("helm template: " + err.Error() + ": " + stderr.String())
	}
	var (
		policies []*admissionv1.ValidatingAdmissionPolicy
		bindings []*admissionv1.ValidatingAdmissionPolicyBinding
	)
	for _, doc := range strings.Split(out.String(), "\n---") {
		var meta metav1.TypeMeta
		Expect(yaml.Unmarshal([]byte(doc), &meta)).To(Succeed())
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			p := &admissionv1.ValidatingAdmissionPolicy{}
			Expect(yaml.UnmarshalStrict([]byte(doc), p)).To(Succeed())
			policies = append(policies, p)
		case "ValidatingAdmissionPolicyBinding":
			b := &admissionv1.ValidatingAdmissionPolicyBinding{}
			Expect(yaml.UnmarshalStrict([]byte(doc), b)).To(Succeed())
			bindings = append(bindings, b)
		case "":
		default:
			Fail("unexpected " + meta.Kind + " in " + workerPolicyTemplate)
		}
	}
	Expect(policies).NotTo(BeEmpty(), "worker policies")
	Expect(bindings).NotTo(BeEmpty(), "worker policy bindings")
	return policies, bindings
}
