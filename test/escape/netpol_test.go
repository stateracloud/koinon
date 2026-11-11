// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// Spike 8.15: a standard Kubernetes NetworkPolicy on the sandbox namespace keeps a bypass of the
// supervisor proxy (a plain pod that opens sockets directly) blocked. The chart ships a
// namespace-wide default-deny (sandboxNetworkPolicy, on by default); OpenShell's own workload
// policy is additive with it. This case applies Koinon's shape when it is missing (as the
// reconciler SA when the chart VAP is present), creates a compliant workload probe pod as the
// Agent Sandbox controller (so the chart's VAP admits it), and asserts the API server is
// unreachable from that pod.
//
//	KOINON_ESCAPE_NAMESPACE     the workspace namespace (required)
//	KOINON_ESCAPE_KUBECONFIG    kubeconfig (default $KUBECONFIG)
//	KOINON_ESCAPE_KUBE_CONTEXT  context (default current)
//	KOINON_ESCAPE_NETPOL        NetworkPolicy name (default <chart ns>-koinon-sandbox-default-deny
//	                            when KOINON_ESCAPE_CHART_NAMESPACE is set, else
//	                            koinon-sandbox-default-deny)
//	KOINON_OPENSHELL_IMAGE      probe image with /bin/sh (required)
//	KOINON_ESCAPE_RUNTIME_CLASS runtime class the pods policy admits (default runc)
//	KOINON_ESCAPE_API           host:port the probe must not reach (default 10.96.0.1:443)
var _ = Describe("Sandbox NetworkPolicy bypass", Label("escape", "openshell"), SpecPriority(-5), func() {
	It("blocks a plain workload pod that opens sockets outside the supervisor", func(ctx SpecContext) {
		testenv.Require(
			testenv.Var("cluster", "KOINON_ESCAPE_NAMESPACE"),
			testenv.Var("probe-image", "KOINON_OPENSHELL_IMAGE"),
		)
		ns := os.Getenv("KOINON_ESCAPE_NAMESPACE")
		image := os.Getenv("KOINON_OPENSHELL_IMAGE")
		runtimeClass := envOr("KOINON_ESCAPE_RUNTIME_CLASS", "runc")
		policyName := envOr("KOINON_ESCAPE_NETPOL", defaultNetPolName())
		apiHost, apiPort := "10.96.0.1", 443
		if t := targets(envOr("KOINON_ESCAPE_API", "10.96.0.1:443")); len(t) > 0 {
			apiHost, apiPort = t[0].host, t[0].port
		}

		cfg := kubeConfig()
		cs := kubernetes.NewForConfigOrDie(cfg)

		By("ensuring Koinon's default-deny NetworkPolicy is in the sandbox namespace")
		writer := &reconciler.SandboxNetworkPolicy{Kube: cs, Name: policyName}
		want := writer.Policy(ns)
		// With the chart, only the reconciler SA may CREATE this object in a sandbox namespace
		// (ValidatingAdmissionPolicy). Bare OpenShell has no such VAP, so cluster-admin is fine.
		createCS := cs
		if chartNS := os.Getenv("KOINON_ESCAPE_CHART_NAMESPACE"); chartNS != "" {
			reconCfg := rest.CopyConfig(cfg)
			reconCfg.Impersonate = rest.ImpersonationConfig{
				UserName: "system:serviceaccount:" + chartNS + ":koinon-reconciler",
				Groups:   []string{"system:serviceaccounts", "system:authenticated"},
			}
			createCS = kubernetes.NewForConfigOrDie(reconCfg)
		}
		_, err := createCS.NetworkingV1().NetworkPolicies(ns).Create(ctx, want, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Fail("cannot create the sandbox default-deny NetworkPolicy: " + err.Error())
		}
		if err == nil {
			DeferCleanup(func(ctx SpecContext) {
				_ = cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, policyName, metav1.DeleteOptions{})
			})
		}
		np, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, policyName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Ingress).To(BeEmpty())
		Expect(np.Spec.Egress).To(BeEmpty())

		By("creating a compliant workload probe pod as the Agent Sandbox controller")
		podName := "koinon-escape-netpol"
		automount := false
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"openshell.ai/boundary-role": "workload", "koinon.dev/escape-netpol": "1"},
			},
			Spec: corev1.PodSpec{
				RuntimeClassName:             &runtimeClass,
				AutomountServiceAccountToken: &automount,
				RestartPolicy:                corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   image,
					Command: []string{"/bin/sleep", "300"},
				}},
			},
		}
		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
		controller := kubernetes.NewForConfigOrDie(impersonated)
		_, err = controller.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			// Without the chart's VAP, cluster-admin creation also works (bare OpenShell).
			_, err = cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		}
		Expect(err).NotTo(HaveOccurred(), "create the NetworkPolicy bypass probe pod")
		DeferCleanup(func(ctx SpecContext) {
			_ = cs.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		})
		Eventually(func() corev1.PodPhase {
			p, getErr := cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
			if getErr != nil {
				return ""
			}
			return p.Status.Phase
		}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(2*time.Second).
			Should(Equal(corev1.PodRunning), "the NetworkPolicy bypass probe pod becomes Running")

		By("the probe cannot reach the API server (kube-network-policies can lag a few seconds)")
		script := "nc -z -w 1 " + apiHost + " " + strconv.Itoa(apiPort) + " && echo open || echo blocked"
		var last string
		Eventually(func(ctx context.Context) string {
			last = kubectlExec(ctx, ns, podName, script)
			if strings.Contains(last, "blocked") {
				return "blocked"
			}
			if strings.Contains(last, "open") {
				return "open"
			}
			return last
		}).WithContext(ctx).WithTimeout(45*time.Second).WithPolling(time.Second).
			Should(Equal("blocked"), "last probe output: %s", last)
	}, NodeTimeout(5*time.Minute))
})

func defaultNetPolName() string {
	if chartNS := os.Getenv("KOINON_ESCAPE_CHART_NAMESPACE"); chartNS != "" {
		return chartNS + "-koinon-sandbox-default-deny"
	}
	return "koinon-sandbox-default-deny"
}

// kubectlExec runs a command in a pod with an explicit kubeconfig and context (AGENTS.md).
func kubectlExec(ctx context.Context, ns, pod, script string) string {
	GinkgoHelper()
	args := []string{"-n", ns, "exec", pod, "--", "/bin/sh", "-c", script}
	if ctxName := os.Getenv("KOINON_ESCAPE_KUBE_CONTEXT"); ctxName != "" {
		args = append([]string{"--context", ctxName}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if kc := os.Getenv("KOINON_ESCAPE_KUBECONFIG"); kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	} else if kc := os.Getenv("KUBECONFIG"); kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	return strings.TrimSpace(out.String())
}
