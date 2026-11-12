// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The Agent Sandbox extension kinds (extensions.agents.x-k8s.io, Agent Sandbox v1.0.7). A
// SandboxClaim makes the controller create a Sandbox (and so pods) from a SandboxTemplate, and a
// SandboxWarmPool keeps pods ready from one: whoever writes them chooses a sandbox pod's shape and
// lifecycle without going through the OpenShell gateway.
var sandboxExtensionGVRs = []schema.GroupVersionResource{
	{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxtemplates"},
	{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"},
	{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"},
}

// Pod fields and object kinds the first admission cases left open (issue 673): added Linux
// capabilities, a seccomp profile other than RuntimeDefault, hostPort, in-place resize
// (pods/resize) and the Agent Sandbox extension kinds. Rendered from charts/koinon with default
// values and enforced by a real API server (envtest), like the cases in admission_test.go. The
// pod fixtures carry the security context OpenShell 0.1.7 sets (driver-kubernetes driver.rs
// apply_supervisor_sandbox_runtime_boundary and sandbox_runtime.rs supervisor_pod): pod seccomp
// RuntimeDefault, every container dropping ALL capabilities and adding none, no host ports.
var _ = Describe("Sandbox admission policy: pod fields and Agent Sandbox kinds", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	var (
		as        map[string]*kubernetes.Clientset
		asDynamic map[string]*dynamic.DynamicClient
		ns        = "kw-escape-fields"
		dryRun    = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
		resizable = "resizable"
	)

	// hardened is the container security context OpenShell 0.1.7 sets on every container it renders.
	hardened := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{
			RunAsNonRoot: new(true), AllowPrivilegeEscalation: new(false),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
	runtimeDefault := func() *corev1.PodSecurityContext {
		return &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	}
	workload := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"openshell.ai/boundary-role": "workload"}},
			Spec: corev1.PodSpec{
				RuntimeClassName:             new("kata"),
				AutomountServiceAccountToken: new(false),
				SecurityContext:              runtimeDefault(),
				InitContainers: []corev1.Container{{
					Name: "bootstrap", Image: "registry.invalid/koinon-escape/agent:1", SecurityContext: hardened(),
				}},
				Containers: []corev1.Container{{
					Name: "agent", Image: "registry.invalid/koinon-escape/agent:1", SecurityContext: hardened(),
					Ports: []corev1.ContainerPort{{Name: "sandbox-control", ContainerPort: 8443, Protocol: corev1.ProtocolTCP}},
				}},
			},
		}
		for _, m := range mutate {
			m(p)
		}
		return p
	}
	supervisor := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		return supervisorPod(name, append([]func(*corev1.Pod){func(p *corev1.Pod) {
			p.Spec.SecurityContext = runtimeDefault()
			p.Spec.Containers[0].SecurityContext = hardened()
		}}, mutate...)...)
	}
	denied := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}
	createPod := func(ctx context.Context, user string, p *corev1.Pod) error {
		_, err := as[user].CoreV1().Pods(ns).Create(ctx, p, dryRun)
		return err
	}
	extension := func(gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
		kind := map[string]string{"sandboxtemplates": "SandboxTemplate", "sandboxclaims": "SandboxClaim", "sandboxwarmpools": "SandboxWarmPool"}[gvr.Resource]
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvr.GroupVersion().String(), "kind": kind,
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"sandboxTemplateRef": map[string]any{"name": "escape"}},
		}}
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderSandboxPolicy(ctx)

		crds := []*apiextensionsv1.CustomResourceDefinition{sandboxCRD()}
		for _, gvr := range sandboxExtensionGVRs {
			crds = append(crds, schemalessCRD(gvr, extension(gvr, "x").GetKind()))
		}
		env := &envtest.Environment{CRDs: crds}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin := kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		asDynamic = map[string]*dynamic.DynamicClient{"admin": dynamic.NewForConfigOrDie(cfg)}
		for _, user := range []string{controllerUser, gatewayUser, otherUser} {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "fields-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
			as[user] = kubernetes.NewForConfigOrDie(c)
			asDynamic[user] = dynamic.NewForConfigOrDie(c)
		}
		_, err = admin.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata"}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: ns, Labels: map[string]string{"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell"},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
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
		Eventually(func() error {
			return createPod(ctx, "admin", workload("probe", func(p *corev1.Pod) { p.Spec.HostNetwork = true }))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))

		By("a sized workload pod the resize cases change")
		_, err = as[controllerUser].CoreV1().Pods(ns).Create(ctx, workload(resizable, func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			}
		}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	It("admits pods in the shape OpenShell 0.1.7 renders (control)", func(ctx SpecContext) {
		Expect(createPod(ctx, controllerUser, workload("ok"))).To(Succeed())
		Expect(createPod(ctx, gatewayUser, supervisor("ok-sup"))).To(Succeed())
		By("a container that repeats the pod's RuntimeDefault profile")
		Expect(createPod(ctx, controllerUser, workload("ok-container-seccomp", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		}))).To(Succeed())
	})

	It("denies added Linux capabilities on any container, for either role", func(ctx SpecContext) {
		const msg = "sandbox pods must not add Linux capabilities"
		add := func(c *corev1.Container, caps ...corev1.Capability) { c.SecurityContext.Capabilities.Add = caps }
		denied(createPod(ctx, controllerUser, workload("cap-agent", func(p *corev1.Pod) { add(&p.Spec.Containers[0], "NET_ADMIN") })), msg)
		denied(createPod(ctx, controllerUser, workload("cap-init", func(p *corev1.Pod) { add(&p.Spec.InitContainers[0], "SYS_ADMIN") })), msg)
		denied(createPod(ctx, controllerUser, workload("cap-nodrop", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_PTRACE"}}
		})), msg)
		denied(createPod(ctx, gatewayUser, supervisor("cap-sup", func(p *corev1.Pod) { add(&p.Spec.Containers[0], "NET_RAW") })), msg)
	})

	It("denies a seccomp profile other than RuntimeDefault, on the pod or on a container", func(ctx SpecContext) {
		const msg = "sandbox pods must use the RuntimeDefault seccomp profile"
		unconfined := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		localhost := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: new("koinon-escape/allow-all.json")}
		denied(createPod(ctx, controllerUser, workload("sc-pod", func(p *corev1.Pod) { p.Spec.SecurityContext.SeccompProfile = unconfined })), msg)
		denied(createPod(ctx, controllerUser, workload("sc-local", func(p *corev1.Pod) { p.Spec.SecurityContext.SeccompProfile = localhost })), msg)
		denied(createPod(ctx, controllerUser, workload("sc-agent", func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.SeccompProfile = unconfined })), msg)
		denied(createPod(ctx, controllerUser, workload("sc-init", func(p *corev1.Pod) { p.Spec.InitContainers[0].SecurityContext.SeccompProfile = localhost })), msg)
		denied(createPod(ctx, gatewayUser, supervisor("sc-sup", func(p *corev1.Pod) { p.Spec.SecurityContext.SeccompProfile = unconfined })), msg)
	})

	It("denies host ports on any container, for either role", func(ctx SpecContext) {
		const msg = "hostPort is not allowed in sandbox pods"
		denied(createPod(ctx, controllerUser, workload("hp-agent", func(p *corev1.Pod) { p.Spec.Containers[0].Ports[0].HostPort = 8443 })), msg)
		denied(createPod(ctx, controllerUser, workload("hp-init", func(p *corev1.Pod) {
			p.Spec.InitContainers[0].Ports = []corev1.ContainerPort{{ContainerPort: 22, HostPort: 2222}}
		})), msg)
		denied(createPod(ctx, gatewayUser, supervisor("hp-sup", func(p *corev1.Pod) {
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 9000, HostPort: 9000}}
		})), msg)
	})

	It("denies an in-place resize (pods/resize) to every identity but the Agent Sandbox controller", func(ctx SpecContext) {
		resize := func(user string) error {
			cur, err := as[user].CoreV1().Pods(ns).Get(ctx, resizable, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			cur.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("8")
			_, err = as[user].CoreV1().Pods(ns).UpdateResize(ctx, resizable, cur, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			return err
		}
		for _, user := range []string{"admin", gatewayUser, otherUser} {
			By(user)
			denied(resize(user), "may change sandbox pod specs")
		}
		By("the controller (control)")
		Expect(resize(controllerUser)).To(Succeed())
	})

	It("denies writes of the Agent Sandbox extension kinds to every identity but the controller", func(ctx SpecContext) {
		for _, gvr := range sandboxExtensionGVRs {
			for _, user := range []string{"admin", gatewayUser, otherUser} {
				By(gvr.Resource + " as " + user)
				_, err := asDynamic[user].Resource(gvr).Namespace(ns).Create(ctx, extension(gvr, "escape"), dryRun)
				denied(err, "only the Agent Sandbox controller may write "+gvr.Resource+" in sandbox namespaces")
			}
			By(gvr.Resource + " as the controller (control)")
			_, err := asDynamic[controllerUser].Resource(gvr).Namespace(ns).Create(ctx, extension(gvr, "escape"), dryRun)
			Expect(err).NotTo(HaveOccurred())
		}

		By("a spec update of an existing claim by another identity")
		claims := sandboxExtensionGVRs[1]
		_, err := asDynamic[controllerUser].Resource(claims).Namespace(ns).Create(ctx, extension(claims, "existing"), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		cur, err := asDynamic["admin"].Resource(claims).Namespace(ns).Get(ctx, "existing", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedField(cur.Object, "other", "spec", "sandboxTemplateRef", "name")).To(Succeed())
		_, err = asDynamic["admin"].Resource(claims).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		denied(err, "only the Agent Sandbox controller may write sandboxclaims")

		By("a metadata-only update by another identity (control)")
		cur, err = asDynamic["admin"].Resource(claims).Namespace(ns).Get(ctx, "existing", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		cur.SetLabels(map[string]string{"koinon.dev/escape": "1"})
		_, err = asDynamic["admin"].Resource(claims).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		Expect(err).NotTo(HaveOccurred())
	})

	It("lists every Agent Sandbox extension kind in the objects policy", func(ctx SpecContext) {
		policies, _ := renderSandboxPolicy(ctx)
		var rules []admissionv1.NamedRuleWithOperations
		for _, p := range policies {
			if strings.HasSuffix(p.Name, "-sandbox-objects") {
				rules = p.Spec.MatchConstraints.ResourceRules
			}
		}
		var got []string
		for _, r := range rules {
			if len(r.APIGroups) == 1 && r.APIGroups[0] == "extensions.agents.x-k8s.io" {
				got = append(got, r.Resources...)
			}
		}
		Expect(got).To(ConsistOf("sandboxtemplates", "sandboxclaims", "sandboxwarmpools"))
	})
})

// schemalessCRD is a schemaless stand-in for an Agent Sandbox CRD: the policies only match on
// group and resource.
func schemalessCRD(gvr schema.GroupVersionResource, kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: gvr.Resource, Singular: strings.ToLower(kind), Kind: kind, ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
			}},
		},
	}
}
