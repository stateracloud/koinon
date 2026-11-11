// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// #76: the reconciler-owned LimitRange sizes unsized containers in OpenShell workspace namespaces.
// The escape workspace is not a Koinon org workspace, so the reconciler never Applies the LimitRange
// there — the suite must CREATE it while impersonating the reconciler SA (same pattern as the
// NetworkPolicy bypass case), then admit a pod with no resources and assert the defaults landed.
//
//	KOINON_ESCAPE_NAMESPACE        the workspace namespace (required)
//	KOINON_ESCAPE_CHART_NAMESPACE  the Koinon release namespace (required; chart e2e only)
//	KOINON_ESCAPE_LIMITRANGE       LimitRange name (default <chart ns>-koinon-sandbox-limitrange)
//	KOINON_OPENSHELL_IMAGE         image for the probe pod (required)
//	KOINON_ESCAPE_RUNTIME_CLASS    runtime class the pods policy admits (default runc)
var _ = Describe("Sandbox LimitRange sizing", Label("escape", "openshell"), SpecPriority(-5), func() {
	It("defaults requests and limits on an unsized container (#76)", func(ctx SpecContext) {
		testenv.Require(
			testenv.Var("cluster", "KOINON_ESCAPE_NAMESPACE"),
			testenv.Var("chart-namespace", "KOINON_ESCAPE_CHART_NAMESPACE"),
			testenv.Var("probe-image", "KOINON_OPENSHELL_IMAGE"),
		)
		ns := os.Getenv("KOINON_ESCAPE_NAMESPACE")
		chartNS := os.Getenv("KOINON_ESCAPE_CHART_NAMESPACE")
		image := os.Getenv("KOINON_OPENSHELL_IMAGE")
		runtimeClass := envOr("KOINON_ESCAPE_RUNTIME_CLASS", "runc")
		name := envOr("KOINON_ESCAPE_LIMITRANGE", defaultLimitRangeName())

		cfg := kubeConfig()
		cs := kubernetes.NewForConfigOrDie(cfg)

		By("ensuring Koinon's LimitRange is in the sandbox namespace (as the reconciler)")
		want := (&reconciler.SandboxLimitRange{Name: name}).LimitRange(ns)
		reconCfg := rest.CopyConfig(cfg)
		reconCfg.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + chartNS + ":koinon-reconciler",
			Groups:   []string{"system:serviceaccounts", "system:authenticated"},
		}
		createCS := kubernetes.NewForConfigOrDie(reconCfg)
		_, err := createCS.CoreV1().LimitRanges(ns).Create(ctx, want, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Fail("cannot create the sandbox LimitRange as the reconciler: " + err.Error())
		}
		if err == nil {
			DeferCleanup(func(ctx SpecContext) {
				_ = cs.CoreV1().LimitRanges(ns).Delete(ctx, name, metav1.DeleteOptions{})
			})
		}
		lr, err := cs.CoreV1().LimitRanges(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(lr.Spec.Limits).To(HaveLen(1))
		Expect(lr.Spec.Limits[0].Type).To(Equal(corev1.LimitTypeContainer))

		By("creating an unsized workload pod the admission policy admits (as the Agent Sandbox controller)")
		podName := "koinon-escape-limitrange-probe"
		automount := false
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"openshell.ai/boundary-role": "workload", "koinon.dev/escape-limitrange": "1"},
			},
			Spec: corev1.PodSpec{
				RuntimeClassName:             &runtimeClass,
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   image,
					Command: []string{"true"},
					// No resources: the LimitRange must supply defaultRequest and default.
				}},
			},
		}
		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
		controller := kubernetes.NewForConfigOrDie(impersonated)
		created, err := controller.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			// Without the chart's VAP, cluster-admin creation also works (bare OpenShell).
			created, err = cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		}
		Expect(err).NotTo(HaveOccurred(), "create the LimitRange sizing probe pod")
		DeferCleanup(func(ctx SpecContext) {
			_ = cs.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		})

		c := created.Spec.Containers[0]
		Expect(c.Resources.Requests).NotTo(BeEmpty(), "LimitRange must supply requests")
		Expect(c.Resources.Limits).NotTo(BeEmpty(), "LimitRange must supply limits")
		cpuReq, memReq := c.Resources.Requests[corev1.ResourceCPU], c.Resources.Requests[corev1.ResourceMemory]
		cpuLim, memLim := c.Resources.Limits[corev1.ResourceCPU], c.Resources.Limits[corev1.ResourceMemory]
		Expect((&cpuReq).Cmp(reconciler.LimitRangeCPURequest)).To(Equal(0))
		Expect((&memReq).Cmp(reconciler.LimitRangeMemoryRequest)).To(Equal(0))
		Expect((&cpuLim).Cmp(reconciler.LimitRangeCPULimit)).To(Equal(0))
		Expect((&memLim).Cmp(reconciler.LimitRangeMemoryLimit)).To(Equal(0))
	}, NodeTimeout(2*time.Minute))
})

func defaultLimitRangeName() string {
	if chartNS := os.Getenv("KOINON_ESCAPE_CHART_NAMESPACE"); chartNS != "" {
		return chartNS + "-koinon-sandbox-limitrange"
	}
	return "koinon-sandbox-limitrange"
}
