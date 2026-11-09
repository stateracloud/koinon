// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// Requesting identities, as charts/koinon/values.yaml names them (sandboxPolicy.identities).
const (
	controllerUser = "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller"
	gatewayUser    = "system:serviceaccount:openshell:openshell"
	// Any other workload identity, for example Koinon's own reconciler.
	otherUser = "system:serviceaccount:koinon:koinon-reconciler"
)

var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// The sandbox-namespace ValidatingAdmissionPolicies (ADR 0045 decision 6, ADR 0048 decision 3)
// exactly as `helm template` renders them from charts/koinon with default values, against a
// real API server (envtest). Pod Security admission is not enforced in envtest's namespaces, so
// every denial below is the VAP's. Needs KUBEBUILDER_ASSETS (setup-envtest) and helm; skips
// without KUBEBUILDER_ASSETS.
var _ = Describe("Sandbox admission policy", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	var (
		admin                   *kubernetes.Clientset
		as                      map[string]*kubernetes.Clientset
		asDynamic               map[string]*dynamic.DynamicClient
		ns                      = "kw-escape"
		plain                   = "koinon-escape-plain"
		dryRun                  = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
		dryRunUpdate            = metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}
		existingPod, existingSc = "existing", "existing"
		policies                []*admissionv1.ValidatingAdmissionPolicy
		bindings                []*admissionv1.ValidatingAdmissionPolicyBinding
	)

	supervisor := supervisorPod
	supervisorToken := supervisorTokenVolume

	// workload is a sandbox workload pod as the Agent Sandbox controller creates it.
	workload := func(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"openshell.ai/boundary-role": "workload"}},
			Spec: corev1.PodSpec{
				RuntimeClassName:             new("kata"),
				AutomountServiceAccountToken: new(false),
				Containers:                   []corev1.Container{{Name: "agent", Image: "registry.invalid/koinon-escape/agent:1"}},
			},
		}
		for _, m := range mutate {
			m(p)
		}
		return p
	}
	// denied asserts that err is a denial by one of Koinon's policies, with message in it.
	denied := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}
	createPod := func(ctx context.Context, user, namespace string, p *corev1.Pod) error {
		_, err := as[user].CoreV1().Pods(namespace).Create(ctx, p, dryRun)
		return err
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings = renderSandboxPolicy(ctx)

		env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{sandboxCRD()}}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)

		as = map[string]*kubernetes.Clientset{"admin": admin}
		asDynamic = map[string]*dynamic.DynamicClient{"admin": dynamic.NewForConfigOrDie(cfg)}
		for _, user := range []string{controllerUser, gatewayUser, otherUser} {
			// Full RBAC rights, so that only admission can say no.
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
			as[user] = kubernetes.NewForConfigOrDie(c)
			asDynamic[user] = dynamic.NewForConfigOrDie(c)
		}
		for name, handler := range map[string]string{"kata": "kata", "kata-qemu": "kata-qemu", "kata-qemu-runtime-rs": "kata-qemu-runtime-rs", "runc": "runc", "gvisor": "runsc"} {
			_, err := admin.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: handler,
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for name, labels := range map[string]map[string]string{
			ns:    {"openshell.ai/managed-by": "openshell"}, // an OpenShell workspace namespace
			plain: nil,
		} {
			_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// No controller manager in envtest: the ServiceAccount admission plugin needs it.
			_, err = admin.CoreV1().ServiceAccounts(name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		for _, p := range policies {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, p, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, b := range bindings {
			_, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, b, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("waiting until the policies are enforced")
		// A pod that breaks every pod rule, so that the wait does not depend on any one of them.
		Eventually(func() error {
			return createPod(ctx, "admin", ns, workload("probe", func(p *corev1.Pod) {
				p.Spec.HostNetwork, p.Spec.RuntimeClassName = true, nil
				p.Annotations = map[string]string{"io.katacontainers.config.runtime.disable_guest_seccomp": "true"}
			}))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))

		By("objects the update cases change")
		_, err = as[controllerUser].CoreV1().Pods(ns).Create(ctx, workload(existingPod), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = as[gatewayUser].CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: existingSc}, StringData: map[string]string{"token": "gateway"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	It("admits a compliant workload pod from the Agent Sandbox controller (control)", func(ctx SpecContext) {
		Expect(createPod(ctx, controllerUser, ns, workload("ok"))).To(Succeed())
		Expect(createPod(ctx, controllerUser, ns, workload("ok-qemu", func(p *corev1.Pod) { p.Spec.RuntimeClassName = new("kata-qemu") }))).To(Succeed())
		Expect(createPod(ctx, controllerUser, ns, workload("ok-qemu-rs", func(p *corev1.Pod) { p.Spec.RuntimeClassName = new("kata-qemu-runtime-rs") }))).To(Succeed())
	})

	It("applies only in sandbox namespaces (control)", func(ctx SpecContext) {
		Expect(createPod(ctx, "admin", plain, workload("plain", func(p *corev1.Pod) {
			p.Spec.HostNetwork = true
			p.Annotations = map[string]string{"io.katacontainers.config.runtime.disable_guest_seccomp": "true"}
		}))).To(Succeed())
	})

	It("denies pod creation by anyone but the Agent Sandbox controller, cluster admins included", func(ctx SpecContext) {
		for _, user := range []string{"admin", gatewayUser, otherUser} {
			denied(createPod(ctx, user, ns, workload("not-controller")), "may create pods in sandbox namespaces, not ")
		}
		By("a pod without the workload label is still a pod in a sandbox namespace")
		denied(createPod(ctx, otherUser, ns, workload("unlabelled", func(p *corev1.Pod) { p.Labels = nil })), "may create pods in sandbox namespaces")
	})

	It("denies io.katacontainers.* annotations, on create and on update", func(ctx SpecContext) {
		kata := map[string]string{"io.katacontainers.config.runtime.disable_guest_seccomp": "true"}
		denied(createPod(ctx, controllerUser, ns, workload("kata-annotation", func(p *corev1.Pod) { p.Annotations = kata })), "io.katacontainers.* annotations are not allowed")

		for _, user := range []string{controllerUser, gatewayUser, otherUser, "admin"} {
			p, err := admin.CoreV1().Pods(ns).Get(ctx, existingPod, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Annotations = kata
			_, err = as[user].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
			denied(err, "io.katacontainers.* annotations are not allowed")
		}
	})

	It("denies workload pods without an allowed runtime class", func(ctx SpecContext) {
		denied(createPod(ctx, controllerUser, ns, workload("runc", func(p *corev1.Pod) { p.Spec.RuntimeClassName = new("runc") })), "must use runtimeClassName kata or kata-qemu or kata-qemu-runtime-rs")
		denied(createPod(ctx, controllerUser, ns, workload("gvisor", func(p *corev1.Pod) { p.Spec.RuntimeClassName = new("gvisor") })), "must use runtimeClassName kata or kata-qemu or kata-qemu-runtime-rs")
		denied(createPod(ctx, controllerUser, ns, workload("default-runtime", func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil })), "must use runtimeClassName kata or kata-qemu or kata-qemu-runtime-rs")
	})

	It("denies host namespaces, hostPath volumes and privileged containers", func(ctx SpecContext) {
		privileged := &corev1.SecurityContext{Privileged: new(true)}
		for _, c := range []struct {
			message string
			mutate  func(*corev1.Pod)
		}{
			{"hostNetwork is not allowed", func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
			{"host PID and IPC namespaces are not allowed", func(p *corev1.Pod) { p.Spec.HostPID = true }},
			{"host PID and IPC namespaces are not allowed", func(p *corev1.Pod) { p.Spec.HostIPC = true }},
			{"hostPath volumes are not allowed", func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
			}},
			{"privileged containers are not allowed", func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext = privileged }},
			{"privileged containers are not allowed", func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "registry.invalid/koinon-escape/init:1", SecurityContext: privileged}}
			}},
		} {
			denied(createPod(ctx, controllerUser, ns, workload("shape", c.mutate)), c.message)
		}
	})

	It("denies a ServiceAccount token on workload pods, the controller's included", func(ctx SpecContext) {
		const mount = "sandbox workload pods must set automountServiceAccountToken to false"
		const project = "sandbox workload pods must not project a ServiceAccount token"
		tokenVolume := corev1.Volume{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
			},
		}}}
		denied(createPod(ctx, controllerUser, ns, workload("automount-true", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = new(true) })), mount)
		denied(createPod(ctx, controllerUser, ns, workload("automount-unset", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil })), mount)
		denied(createPod(ctx, controllerUser, ns, workload("projected-token", func(p *corev1.Pod) {
			p.Spec.ServiceAccountName = "default" // the API server rejects a token projection without one
			p.Spec.Volumes = []corev1.Volume{tokenVolume}
		})), project)

		By("a projected volume without a token is fine (control)")
		Expect(createPod(ctx, controllerUser, ns, workload("projected-config", func(p *corev1.Pod) {
			v := tokenVolume.DeepCopy()
			v.Projected.Sources = v.Projected.Sources[:1]
			p.Spec.Volumes = []corev1.Volume{*v}
		}))).To(Succeed())

		By("supervisor pods: automount false and only a bounded openshell-gateway token")
		Expect(createPod(ctx, gatewayUser, ns, supervisor("supervisor-real-shape"))).To(Succeed())
		supMount := "sandbox supervisor pods must set automountServiceAccountToken to false"
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-automount-unset", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil })), supMount)
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-automount-true", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = new(true) })), supMount)
		const supAud = "may project only a ServiceAccount token for the audience openshell-gateway"
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-no-audience", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{supervisorToken("", 3600)} })), supAud)
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-api-audience", func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{supervisorToken("https://kubernetes.default.svc", 3600)}
		})), supAud)
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-mixed-sources", func(p *corev1.Pod) {
			// The real gateway token next to an API-audience token in the same sources list.
			v := supervisorToken("openshell-gateway", 3600)
			exp := int64(3600)
			v.Projected.Sources = append(v.Projected.Sources, corev1.VolumeProjection{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: "https://kubernetes.default.svc", ExpirationSeconds: &exp, Path: "api-token",
			}})
			p.Spec.Volumes = []corev1.Volume{v}
		})), supAud)
		denied(createPod(ctx, gatewayUser, ns, supervisor("sup-long-token", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{supervisorToken("openshell-gateway", 86400)} })), "must bound the ServiceAccount token to 3600 seconds")
	})

	It("denies ephemeral containers, the controller's included", func(ctx SpecContext) {
		for _, user := range []string{controllerUser, gatewayUser, otherUser, "admin"} {
			p, err := admin.CoreV1().Pods(ns).Get(ctx, existingPod, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.invalid/koinon-escape/debug:1"},
			}}
			_, err = as[user].CoreV1().Pods(ns).UpdateEphemeralContainers(ctx, existingPod, p, dryRunUpdate)
			denied(err, "ephemeral containers are not allowed")
		}
	})

	It("denies pod spec changes by anyone but the controller", func(ctx SpecContext) {
		for _, user := range []string{"admin", gatewayUser, otherUser} {
			p, err := admin.CoreV1().Pods(ns).Get(ctx, existingPod, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.ActiveDeadlineSeconds = new(int64(60))
			_, err = as[user].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
			denied(err, "may change sandbox pod specs")
		}

		By("metadata-only updates stay open (control)")
		p, err := admin.CoreV1().Pods(ns).Get(ctx, existingPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		p.Labels["koinon-escape"] = "label"
		_, err = admin.CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
		Expect(err).NotTo(HaveOccurred())
	})

	// connect sends a CONNECT-class request (exec, attach, portforward, pods/proxy, services/proxy)
	// as user and returns the error. Admission runs before the API server opens the connection, so
	// this needs no kubelet: a denial is the VAP's, anything else (no node, no such Service) is what
	// happens after admission.
	connect := func(ctx context.Context, user, namespace, resource, name, sub string) error {
		GinkgoHelper()
		var req *rest.Request
		switch sub {
		case "exec":
			req = as[user].CoreV1().RESTClient().Post().Param("command", "id").Param("stdout", "true")
		case "attach":
			req = as[user].CoreV1().RESTClient().Post().Param("stdout", "true")
		case "portforward":
			req = as[user].CoreV1().RESTClient().Post().Param("ports", "8080")
		default:
			req = as[user].CoreV1().RESTClient().Get()
		}
		return req.Namespace(namespace).Resource(resource).Name(name).SubResource(sub).Do(ctx).Error()
	}
	connectCases := []struct{ resource, sub string }{
		{"pods", "exec"}, {"pods", "attach"}, {"pods", "portforward"}, {"pods", "proxy"}, {"services", "proxy"},
	}

	It("denies exec, attach, port-forward and proxy into sandbox pods, for every identity", func(ctx SpecContext) {
		for _, user := range []string{"admin", controllerUser, gatewayUser, otherUser} {
			for _, c := range connectCases {
				name := existingPod
				if c.resource == "services" {
					name = "some-service"
				}
				err := connect(ctx, user, ns, c.resource, name, c.sub)
				denied(err, c.resource+"/"+c.sub+" is not allowed in sandbox namespaces")
				Expect(err.Error()).To(ContainSubstring(user))
				Expect(err.Error()).To(ContainSubstring("docs/ops/sandbox-break-glass.md"))
			}
		}
	})

	It("leaves exec and proxy open in other namespaces (control)", func(ctx SpecContext) {
		_, err := admin.CoreV1().Pods(plain).Create(ctx, workload("plain-connect", func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil }), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, c := range connectCases {
			name := "plain-connect"
			if c.resource == "services" {
				name = "some-service"
			}
			err := connect(ctx, "admin", plain, c.resource, name, c.sub)
			// Past admission it fails for want of a node or a Service, never on the policy.
			if err != nil {
				Expect(err.Error()).NotTo(ContainSubstring("ValidatingAdmissionPolicy"), "%s/%s", c.resource, c.sub)
			}
		}
	})

	It("enforces the policies: Deny actions, failurePolicy Fail, bound to OpenShell namespaces only", func() {
		for _, p := range policies {
			Expect(p.Spec.FailurePolicy).To(HaveValue(Equal(admissionv1.Fail)), p.Name)
		}
		for _, b := range bindings {
			Expect(b.Spec.ValidationActions).To(Equal([]admissionv1.ValidationAction{admissionv1.Deny}), b.Name)
			Expect(b.Spec.MatchResources).NotTo(BeNil(), b.Name)
			Expect(b.Spec.MatchResources.NamespaceSelector.MatchLabels).To(HaveKeyWithValue("openshell.ai/managed-by", "openshell"), b.Name)
			// An options object has no labels: an objectSelector would silently match nothing.
			Expect(b.Spec.MatchResources.ObjectSelector).To(BeNil(), b.Name)
		}

		By("a policy denies CONNECT on exec, attach, portforward and both proxies, with no exemption")
		var rules []admissionv1.NamedRuleWithOperations
		for _, p := range policies {
			if p.Spec.MatchConstraints == nil {
				continue
			}
			for _, r := range p.Spec.MatchConstraints.ResourceRules {
				if slices.Contains(r.Operations, admissionv1.Connect) {
					rules = append(rules, r)
					Expect(p.Spec.Validations).To(HaveLen(1), p.Name)
					Expect(p.Spec.Validations[0].Expression).To(Equal("false"), p.Name)
					Expect(p.Spec.Variables).To(BeEmpty(), p.Name)
				}
			}
		}
		Expect(rules).To(HaveLen(1), "exactly one CONNECT rule")
		Expect(rules[0].Operations).To(Equal([]admissionv1.OperationType{admissionv1.Connect}))
		Expect(rules[0].Resources).To(ConsistOf("pods/exec", "pods/attach", "pods/portforward", "pods/proxy", "services/proxy"))
	})

	It("denies every io.katacontainers.* annotation on every pod kind, not one known key", func(ctx SpecContext) {
		for _, key := range []string{
			"io.katacontainers.config.runtime.disable_guest_seccomp",
			"io.katacontainers.config.hypervisor.kernel_params",
			"io.katacontainers.config.agent.kernel_modules",
			"io.katacontainers.config.hypervisor.default_memory",
		} {
			kata := map[string]string{key: "true"}
			denied(createPod(ctx, controllerUser, ns, workload("kata-workload", func(p *corev1.Pod) { p.Annotations = kata })), "io.katacontainers.* annotations are not allowed")
			denied(createPod(ctx, gatewayUser, ns, supervisor("kata-supervisor", func(p *corev1.Pod) { p.Annotations = kata })), "io.katacontainers.* annotations are not allowed")
		}
		By("annotations that merely mention Kata are untouched (control)")
		Expect(createPod(ctx, controllerUser, ns, workload("kata-lookalike", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"example.com/io.katacontainers.config": "x", "koinon.dev/runtime": "kata"}
		}))).To(Succeed())
	})

	It("lets the gateway create supervisor pods with the pinned image only", func(ctx SpecContext) {
		Expect(createPod(ctx, gatewayUser, ns, supervisor("sup-ok"))).To(Succeed())
		for _, c := range []struct {
			message string
			mutate  func(*corev1.Pod)
		}{
			{"must use the pinned supervisor image", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/evil:1" }},
			{"must use the pinned supervisor image", func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "registry.invalid/koinon-escape/evil:1"}}
			}},
			{"hostNetwork is not allowed", func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
			{"hostPath volumes are not allowed", func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
			}},
			{"privileged containers are not allowed", func(p *corev1.Pod) {
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: new(true)}
			}},
		} {
			denied(createPod(ctx, gatewayUser, ns, supervisor("sup-bad", c.mutate)), c.message)
		}
		By("the gateway may not create a workload pod, nor may anyone else create a supervisor pod")
		denied(createPod(ctx, gatewayUser, ns, workload("gw-workload")), "may create pods in sandbox namespaces")
		for _, user := range []string{"admin", otherUser} {
			denied(createPod(ctx, user, ns, supervisor("sup-other")), "may create pods in sandbox namespaces")
		}
	})

	It("lets the gateway change a supervisor pod only by removing scheduling gates", func(ctx SpecContext) {
		_, err := as[gatewayUser].CoreV1().Pods(ns).Create(ctx, supervisor("sup-gated"), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		p, err := admin.CoreV1().Pods(ns).Get(ctx, "sup-gated", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		p.Spec.SchedulingGates = nil
		p.Spec.ActiveDeadlineSeconds = new(int64(60))
		_, err = as[gatewayUser].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
		denied(err, "may change sandbox pod specs")

		p, err = admin.CoreV1().Pods(ns).Get(ctx, "sup-gated", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		p.Spec.SchedulingGates = nil
		_, err = as[otherUser].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
		denied(err, "may change sandbox pod specs")
		_, err = as[gatewayUser].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
		Expect(err).NotTo(HaveOccurred(), "removing the scheduling gate is what the gateway needs")

		By("not by swapping the container image, with or without the gate")
		for _, ungate := range []bool{false, true} {
			p, err = admin.CoreV1().Pods(ns).Get(ctx, "sup-gated", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.Containers[0].Image = "registry.invalid/koinon-escape/evil:1"
			if ungate {
				p.Spec.SchedulingGates = nil
			}
			_, err = as[gatewayUser].CoreV1().Pods(ns).Update(ctx, p, dryRunUpdate)
			denied(err, "may change sandbox pod specs")
		}
	})

	It("lets only the OpenShell gateway write Secrets in sandbox namespaces", func(ctx SpecContext) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "planted"}, StringData: map[string]string{"token": "planted"}}
		for _, user := range []string{"admin", controllerUser, otherUser} {
			_, err := as[user].CoreV1().Secrets(ns).Create(ctx, secret, dryRun)
			denied(err, "only the OpenShell gateway may write secrets in sandbox namespaces")
		}
		_, err := as[gatewayUser].CoreV1().Secrets(ns).Create(ctx, secret, dryRun)
		Expect(err).NotTo(HaveOccurred())

		By("changing an existing Secret's data")
		s, err := admin.CoreV1().Secrets(ns).Get(ctx, existingSc, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		s.Data["token"] = []byte("replaced")
		_, err = admin.CoreV1().Secrets(ns).Update(ctx, s, dryRunUpdate)
		denied(err, "only the OpenShell gateway may write secrets")

		By("Secrets outside sandbox namespaces are not affected (control)")
		_, err = admin.CoreV1().Secrets(plain).Create(ctx, secret, dryRun)
		Expect(err).NotTo(HaveOccurred())
	})

	It("lets only the OpenShell gateway and the controller write Sandbox objects", func(ctx SpecContext) {
		sb := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": sandboxGVR.GroupVersion().String(), "kind": "Sandbox",
			"metadata": map[string]any{"name": "planted"},
			"spec":     map[string]any{"podTemplate": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "agent", "image": "registry.invalid/x:1"}}}}},
		}}
		for _, user := range []string{"admin", otherUser} {
			_, err := asDynamic[user].Resource(sandboxGVR).Namespace(ns).Create(ctx, sb, dryRun)
			denied(err, "only the OpenShell gateway may write sandboxes in sandbox namespaces")
		}
		for _, user := range []string{gatewayUser, controllerUser} {
			_, err := asDynamic[user].Resource(sandboxGVR).Namespace(ns).Create(ctx, sb, dryRun)
			Expect(err).NotTo(HaveOccurred(), user)
		}
	})
})

// supervisorTokenVolume is the projected volume OpenShell 0.1.5 gives a supervisor pod (sa-token).
func supervisorTokenVolume(aud string, exp int64) corev1.Volume {
	return corev1.Volume{Name: "sa-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: aud, ExpirationSeconds: &exp, Path: "token"}}},
	}}}
}

// supervisorPod is an OpenShell supervisor pod as the gateway creates it (chart default image):
// OpenShell 0.1.5 sets automount false and projects one openshell-gateway token (sa-token).
// Shared by every admission spec so the fixture cannot drift from the real shape.
func supervisorPod(name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"openshell.ai/boundary-role": "supervisor"}},
		Spec: corev1.PodSpec{
			SchedulingGates:              []corev1.PodSchedulingGate{{Name: "openshell.ai/bootstrap"}},
			ServiceAccountName:           "default",
			AutomountServiceAccountToken: new(false),
			Containers:                   []corev1.Container{{Name: "supervisor", Image: "ghcr.io/nvidia/openshell/supervisor:0.1.5"}},
			Volumes:                      []corev1.Volume{supervisorTokenVolume("openshell-gateway", 3600)},
		},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

// renderSandboxPolicy renders charts/koinon's sandbox-policy template with default values, plus
// any extra helm arguments.
func renderSandboxPolicy(ctx context.Context, extraArgs ...string) ([]*admissionv1.ValidatingAdmissionPolicy, []*admissionv1.ValidatingAdmissionPolicyBinding) {
	GinkgoHelper()
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	Expect(err).NotTo(HaveOccurred(), "helm renders the chart's policies")
	var out, stderr bytes.Buffer
	args := append([]string{
		"template", "koinon", "../../charts/koinon", "--namespace", "koinon",
		"--set", "openshell.insecure=true", "--set", "openshell.address=openshell.openshell.svc:8080", "--show-only", "templates/sandbox-policy.yaml",
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, helm, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	Expect(cmd.Run()).To(Succeed(), stderr.String())

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
			Fail(fmt.Sprintf("unexpected %s in sandbox-policy.yaml", meta.Kind))
		}
	}
	Expect(policies).To(HaveLen(3), "the pods, objects and connect policies")
	Expect(bindings).To(HaveLen(3))
	return policies, bindings
}

// sandboxCRD is a schemaless stand-in for Agent Sandbox's Sandbox CRD: the objects policy only
// matches on group and resource.
func sandboxCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxGVR.Resource + "." + sandboxGVR.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: sandboxGVR.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "sandboxes", Singular: "sandbox", Kind: "Sandbox", ListKind: "SandboxList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: sandboxGVR.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
			}},
		},
	}
}
