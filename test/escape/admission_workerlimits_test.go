// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
// quota controller does, and the API server then counts every admitted pod.
var _ = Describe("Worker namespace limits", Label("escape", "admission", "substrate"), Ordered, func() {
	var cs *kubernetes.Clientset
	const ns = "limits-workers"

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
			"--set", "execution.substrate.routerAddress=atenet-router.ate-system.svc:80",
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

		var quota *corev1.ResourceQuota
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
		Expect(res.Requests.Cpu().String()).To(Equal("250m"))
		Expect(res.Requests.Memory().String()).To(Equal("256Mi"))
		Expect(res.Limits.Cpu().String()).To(Equal("2"))
		Expect(res.Limits.Memory().String()).To(Equal("2Gi"))
	})

	It("refuses a pod that goes over the CPU quota", func(ctx SpecContext) {
		// Requests of 2 CPU against a quota of 1.
		_, err := cs.CoreV1().Pods(ns).Create(ctx, pod("big", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		}), metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want a quota refusal, got %v", err)
		Expect(err.Error()).To(ContainSubstring("exceeded quota"))
	})

	It("refuses a pod over the pod count", func(ctx SpecContext) {
		small := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		}
		// "unsized" is the first pod (250m of the 1 CPU). The second fits. The third is over the count of two.
		_, err := cs.CoreV1().Pods(ns).Create(ctx, pod("second", small), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(ns).Create(ctx, pod("third", small), metav1.CreateOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want a quota refusal, got %v", err)
		Expect(err.Error()).To(ContainSubstring("exceeded quota"))
		Expect(err.Error()).To(ContainSubstring("pods"))
	})
})
