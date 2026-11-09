// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"os"
	"slices"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The chart's sandbox admission policy on a real cluster, for a sandbox namespace that has it
// (ADR 0045 decision 6). The envtest specs in admission_test.go cover the rules; this one covers
// that the cluster enforces them where sandboxes run. Requests are server side dry runs made as
// the Agent Sandbox controller, so the rules under test are the runtime class and the Kata
// annotations, not the one that limits who may create pods. Nothing is created. A maintainer cluster's
// dev profile does not install the chart's policies, and the spec skips there.
//
//	KOINON_ESCAPE_NAMESPACE     the namespace of the OpenShell workspace under test (required)
//	KOINON_ESCAPE_KUBECONFIG    kubeconfig (default $KUBECONFIG, then the default loading rules)
//	KOINON_ESCAPE_KUBE_CONTEXT  context in it (default the current one)
//	KOINON_ESCAPE_POLICY        the chart's pods policy (default koinon-koinon-sandbox-pods, that
//	                            is <release namespace>-<fullname>-sandbox-pods); it must also
//	                            carry the chart's app.kubernetes.io/part-of=koinon label
//	KOINON_ESCAPE_POLICY_REQUIRED  1: the policy must be bound (the spec fails instead of skipping)
//	                            and the namespace must exist. The kind e2e sets it (test/e2e/escape-openshell.sh,
//	                            #109): there the chart is installed with sandboxPolicy enabled.
//	KOINON_ESCAPE_RUNTIME_CLASS  the runtime class the install's policy admits for workload pods
//	                            (default kata; the kind e2e install uses runc, and kind.sh sets it)
//	KOINON_ESCAPE_SUPERVISOR_IMAGE  the supervisor image the install's policy admits (default
//	                            ghcr.io/nvidia/openshell/supervisor:0.1.5)
//
// The container has the lowest priority: the namespace of a workspace exists once a sandbox was created in it,
// and the other specs of the label do that.
var _ = Describe("Sandbox admission policy on the cluster", Label("escape", "openshell"), SpecPriority(-10), func() {
	It("denies Kata annotations and other runtimes for the controller, and admits a compliant pod", func(ctx SpecContext) {
		testenv.Require(testenv.Var("cluster", "KOINON_ESCAPE_NAMESPACE"))
		ns := os.Getenv("KOINON_ESCAPE_NAMESPACE")
		required := os.Getenv("KOINON_ESCAPE_POLICY_REQUIRED") == "1"
		cfg := kubeConfig()
		cs := kubernetes.NewForConfigOrDie(cfg)
		policy := envOr("KOINON_ESCAPE_POLICY", "koinon-koinon-sandbox-pods")
		if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil && required {
			Fail("the workspace namespace " + ns + " does not exist, so no sandbox of this run was created in it: " + err.Error())
		}
		if !policyBound(ctx, cs, policy, ns) {
			msg := "the chart's policy " + policy + " is not installed with a Deny binding that selects namespace " + ns
			if required {
				Fail(msg + " (KOINON_ESCAPE_POLICY_REQUIRED=1: is sandboxPolicy.enabled and the release namespace right?)")
			}
			Skip(msg)
		}

		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
		controller := kubernetes.NewForConfigOrDie(impersonated)

		runtimeClass := envOr("KOINON_ESCAPE_RUNTIME_CLASS", "kata")
		// The RuntimeClass admission plugin runs before the policy and refuses a class that does not exist,
		// so the wrong class must be a real object: a throwaway one, removed at the end (dry runs create nothing).
		const wrongClass = "koinon-escape-wrong"
		_, err := cs.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: wrongClass}, Handler: "runc",
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Fail("cannot create the throwaway RuntimeClass " + wrongClass + " for the wrong-runtime-class case: " + err.Error())
		}
		if err == nil { // remove it only when this run created it
			DeferCleanup(func(ctx SpecContext) {
				_ = cs.NodeV1().RuntimeClasses().Delete(ctx, wrongClass, metav1.DeleteOptions{})
			})
		}

		pod := func(mutate func(*corev1.Pod)) *corev1.Pod {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "koinon-escape-", Labels: map[string]string{"openshell.ai/boundary-role": "workload"}},
				Spec: corev1.PodSpec{
					RuntimeClassName:             &runtimeClass,
					AutomountServiceAccountToken: new(false),
					Containers:                   []corev1.Container{{Name: "agent", Image: "registry.invalid/koinon-escape/agent:1"}},
				},
			}
			mutate(p)
			return p
		}
		dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

		_, err = controller.CoreV1().Pods(ns).Create(ctx, pod(func(*corev1.Pod) {}), dry)
		Expect(err).NotTo(HaveOccurred(), "control: a compliant workload pod is admitted")

		for name, c := range map[string]struct {
			mutate  func(*corev1.Pod)
			message string
		}{
			"no runtime class":    {func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil }, "must use runtimeClassName"},
			"wrong runtime class": {func(p *corev1.Pod) { p.Spec.RuntimeClassName = new(wrongClass) }, "must use runtimeClassName"},
			"kata annotation": {func(p *corev1.Pod) {
				p.Annotations = map[string]string{"io.katacontainers.config.runtime.disable_guest_seccomp": "true"}
			}, "io.katacontainers.* annotations"},
		} {
			_, err := controller.CoreV1().Pods(ns).Create(ctx, pod(c.mutate), dry)
			Expect(err).To(HaveOccurred(), "%s: a dry-run pod was admitted", name)
			Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy '"+policy+"'"), name)
			Expect(err.Error()).To(ContainSubstring(c.message), name)
		}

		// The supervisor rules (ADR 0006 amendment 2026-11-09, #220), as the OpenShell gateway: a
		// copy of the supervisor pod shape (supervisorPod) is admitted, and a token for the API server
		// or a long-lived one is not. This is a dry run of a copy; that the pods the gateway really
		// made carry the labels and the shape these rules select is checked by realPods below, in the
		// "Agent sandbox" container's "pods OpenShell made" spec, while a sandbox is up.
		gateway := rest.CopyConfig(cfg)
		gateway.Impersonate = rest.ImpersonationConfig{UserName: gatewayUser, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
		gw := kubernetes.NewForConfigOrDie(gateway)
		image := envOr("KOINON_ESCAPE_SUPERVISOR_IMAGE", "ghcr.io/nvidia/openshell/supervisor:0.1.5")
		supervisor := func(mutate func(*corev1.Pod)) *corev1.Pod {
			// The default image comes first, so a mutation can override it.
			p := supervisorPod("koinon-escape-sup", func(p *corev1.Pod) { p.Spec.Containers[0].Image = image }, mutate)
			p.Name = ""
			p.GenerateName = "koinon-escape-sup-"
			return p
		}
		_, err = gw.CoreV1().Pods(ns).Create(ctx, supervisor(func(*corev1.Pod) {}), dry)
		Expect(err).NotTo(HaveOccurred(), "control: a supervisor pod in the shape OpenShell creates is admitted for the gateway")
		const supAud = "may project only a ServiceAccount token for the audience openshell-gateway"
		for name, c := range map[string]struct {
			mutate  func(*corev1.Pod)
			message string
		}{
			"api server audience": {func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{supervisorTokenVolume("https://kubernetes.default.svc", 3600)}
			}, supAud},
			"no audience": {func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{supervisorTokenVolume("", 3600)} }, supAud},
			"long token": {func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{supervisorTokenVolume("openshell-gateway", 86400)}
			}, "must bound the ServiceAccount token"},
			"token mounted by default": {func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil }, "sandbox supervisor pods must set automountServiceAccountToken to false"},
			"other image":              {func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/supervisor:1" }, "must use the pinned supervisor image"},
		} {
			_, err := gw.CoreV1().Pods(ns).Create(ctx, supervisor(c.mutate), dry)
			Expect(err).To(HaveOccurred(), "%s: a dry-run supervisor pod was admitted", name)
			Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy '"+policy+"'"), name)
			Expect(err.Error()).To(ContainSubstring(c.message), name)
		}
	})
})

// kubeConfig is the cluster client configuration for the live cases, or nil without
// KOINON_ESCAPE_NAMESPACE.
func kubeConfig() *rest.Config {
	GinkgoHelper()
	if os.Getenv("KOINON_ESCAPE_NAMESPACE") == "" {
		return nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if f := os.Getenv("KOINON_ESCAPE_KUBECONFIG"); f != "" {
		rules.ExplicitPath = f
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: os.Getenv("KOINON_ESCAPE_KUBE_CONTEXT")}).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	return cfg
}

// kubeClient is the cluster client for the live cases, or nil without KOINON_ESCAPE_NAMESPACE.
func kubeClient() kubernetes.Interface {
	GinkgoHelper()
	cfg := kubeConfig()
	if cfg == nil {
		return nil
	}
	return kubernetes.NewForConfigOrDie(cfg)
}

// policyBound reports whether the chart's policy (by exact name and the chart's part-of label)
// has a binding with the Deny action that selects the namespace.
func policyBound(ctx context.Context, cs kubernetes.Interface, policy, ns string) bool {
	GinkgoHelper()
	p, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy, metav1.GetOptions{})
	if err != nil || p.Labels["app.kubernetes.io/part-of"] != "koinon" {
		return false
	}
	namespace, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	list, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, b := range list.Items {
		if b.Spec.PolicyName != policy || b.Spec.MatchResources == nil || b.Spec.MatchResources.NamespaceSelector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(b.Spec.MatchResources.NamespaceSelector)
		Expect(err).NotTo(HaveOccurred())
		for _, a := range b.Spec.ValidationActions {
			if a == admissionv1.Deny && sel.Matches(labels.Set(namespace.Labels)) {
				return true
			}
		}
	}
	return false
}

// realPods checks the pods OpenShell and the Agent Sandbox controller made in the workspace
// namespace against what the chart's policy selects and expects (#109): the policy's supervisor
// rules only apply to a pod labelled openshell.ai/boundary-role=supervisor, so a real supervisor
// pod without that label, or in another shape, would pass every dry run of a copy. Call it while a
// sandbox runs. A missing pod fails when KOINON_ESCAPE_POLICY_REQUIRED=1 and skips otherwise.
//
//	KOINON_ESCAPE_SUPERVISOR_TOKEN_MAX  the chart's sandboxPolicy.supervisorTokenMaxSeconds (default 3600)
func realPods(ctx context.Context, cs kubernetes.Interface, ns string) {
	GinkgoHelper()
	required := os.Getenv("KOINON_ESCAPE_POLICY_REQUIRED") == "1"
	missing := func(msg string) {
		GinkgoHelper()
		if required {
			Fail(msg + " (KOINON_ESCAPE_POLICY_REQUIRED=1)")
		}
		Skip(msg)
	}
	maxSeconds := int64(3600)
	if v := os.Getenv("KOINON_ESCAPE_SUPERVISOR_TOKEN_MAX"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		Expect(err).NotTo(HaveOccurred(), "KOINON_ESCAPE_SUPERVISOR_TOKEN_MAX")
		maxSeconds = n
	}
	image := envOr("KOINON_ESCAPE_SUPERVISOR_IMAGE", "ghcr.io/nvidia/openshell/supervisor:0.1.5")

	running := func(role string) []corev1.Pod {
		GinkgoHelper()
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "openshell.ai/boundary-role=" + role})
		Expect(err).NotTo(HaveOccurred())
		var out []corev1.Pod
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodRunning {
				out = append(out, p)
			}
		}
		return out
	}
	supervisors, workloads := running("supervisor"), running("workload")
	if len(supervisors) == 0 {
		missing("no Running pod with openshell.ai/boundary-role=supervisor in namespace " + ns + ": the policy's supervisor rules would not apply to the pods OpenShell makes")
	}
	if len(workloads) == 0 {
		missing("no Running pod with openshell.ai/boundary-role=workload in namespace " + ns + ": the policy's workload rules would not apply to the pods the controller makes")
	}
	for _, p := range supervisors {
		spec := p.Spec
		Expect(spec.AutomountServiceAccountToken).NotTo(BeNil(), p.Name)
		Expect(*spec.AutomountServiceAccountToken).To(BeFalse(), p.Name+": automountServiceAccountToken")
		var tokens []corev1.ServiceAccountTokenProjection
		for _, v := range spec.Volumes {
			if v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ServiceAccountToken != nil {
					tokens = append(tokens, *s.ServiceAccountToken)
				}
			}
		}
		Expect(tokens).To(HaveLen(1), p.Name+": projected ServiceAccount tokens")
		Expect(tokens[0].Audience).To(Equal("openshell-gateway"), p.Name)
		Expect(tokens[0].ExpirationSeconds).NotTo(BeNil(), p.Name)
		Expect(*tokens[0].ExpirationSeconds).To(BeNumerically("<=", maxSeconds), p.Name+": expirationSeconds")
		for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
			Expect(c.Image).To(Equal(image), p.Name+"/"+c.Name+": the pinned supervisor image")
		}
	}
}
