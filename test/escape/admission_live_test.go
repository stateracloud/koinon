// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The sandbox admission policies on a real cluster, for a sandbox namespace that has them (the
// chart's sandboxPolicy, ADR 0045 decision 6). The envtest specs in admission_test.go cover the
// rules; this one covers that the cluster enforces them where sandboxes run. Requests are server
// side dry runs, so nothing is created. The home lab's dev profile does not install the chart's
// policies, and the spec skips there.
//
//	KOINON_ESCAPE_NAMESPACE    the namespace of the OpenShell workspace under test (required)
//	KOINON_ESCAPE_KUBECONFIG   kubeconfig (default $KUBECONFIG, then the default loading rules)
//	KOINON_ESCAPE_KUBE_CONTEXT context in it (default the current one)
var _ = Describe("Sandbox admission policy on the cluster", Label("escape", "openshell"), func() {
	It("denies pods that are not the controller's, Kata annotations and other runtimes", func(ctx SpecContext) {
		ns := os.Getenv("KOINON_ESCAPE_NAMESPACE")
		if ns == "" {
			Skip("KOINON_ESCAPE_NAMESPACE is not set")
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if f := os.Getenv("KOINON_ESCAPE_KUBECONFIG"); f != "" {
			rules.ExplicitPath = f
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
			&clientcmd.ConfigOverrides{CurrentContext: os.Getenv("KOINON_ESCAPE_KUBE_CONTEXT")}).ClientConfig()
		Expect(err).NotTo(HaveOccurred())
		cs := kubernetes.NewForConfigOrDie(cfg)

		if !sandboxPolicyBound(ctx, cs, ns) {
			Skip("no sandbox ValidatingAdmissionPolicy (*-sandbox-pods) is bound to namespace " + ns + ": the chart's sandboxPolicy is not installed")
		}

		pod := func(mutate func(*corev1.Pod)) *corev1.Pod {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "koinon-escape-", Labels: map[string]string{"openshell.ai/boundary-role": "workload"}},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false),
					Containers:                   []corev1.Container{{Name: "agent", Image: "registry.invalid/koinon-escape/agent:1"}},
				},
			}
			mutate(p)
			return p
		}
		dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
		for name, mutate := range map[string]func(*corev1.Pod){
			// Pod Security (baseline) would refuse hostNetwork before the policy is asked, so the
			// cases are the ones it does not cover.
			"no runtime class": func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil },
			"kata annotation": func(p *corev1.Pod) {
				p.Annotations = map[string]string{"io.katacontainers.config.runtime.disable_guest_seccomp": "true"}
				p.Spec.RuntimeClassName = new("kata")
			},
		} {
			_, err := cs.CoreV1().Pods(ns).Create(ctx, pod(mutate), dry)
			Expect(err).To(HaveOccurred(), "%s: a dry-run pod was admitted", name)
			Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"), name)
		}
	})
})

// sandboxPolicyBound reports whether a ValidatingAdmissionPolicyBinding of a policy named
// *-sandbox-pods selects the namespace.
func sandboxPolicyBound(ctx context.Context, cs kubernetes.Interface, ns string) bool {
	GinkgoHelper()
	namespace, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	list, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, b := range list.Items {
		if !strings.HasSuffix(b.Spec.PolicyName, "-sandbox-pods") || b.Spec.MatchResources == nil || b.Spec.MatchResources.NamespaceSelector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(b.Spec.MatchResources.NamespaceSelector)
		Expect(err).NotTo(HaveOccurred())
		if sel.Matches(labels.Set(namespace.Labels)) && hasAction(b.Spec.ValidationActions, admissionv1.Deny) {
			return true
		}
	}
	return false
}

func hasAction(actions []admissionv1.ValidationAction, a admissionv1.ValidationAction) bool {
	for _, x := range actions {
		if x == a {
			return true
		}
	}
	return false
}
