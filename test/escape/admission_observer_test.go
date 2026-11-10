// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The sandbox observer's RBAC and the policy that fences it (ADR 0078), exactly as `helm template`
// renders templates/sandbox-observer.yaml with default values, against a real API server (envtest,
// RBAC enforced). The reconciler may bind the observer ClusterRole only in OpenShell workspace
// namespaces of its gateway, with the fixed name, role and subject; nobody else may; and what the
// binding grants is get on pods and Sandboxes there, to the reconciler alone.
var _ = Describe("Sandbox observer admission policy", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	const (
		role     = "koinon-koinon-sandbox-observer" // the ClusterRole, and the RoleBinding's name
		managed  = "openshell-openshell-kw-obs"     // a workspace namespace of gateway "openshell"
		noWs     = "openshell-openshell-kw-nows"    // labelled like one, without the sandbox-workspace label
		badName  = "kw-obs-misnamed"                // labelled like one, but not named openshell-openshell-*
		foreign  = "openshell-prod-kw-obs"          // a workspace namespace of another gateway
		plain    = "koinon-observer-plain"          // not an OpenShell namespace
		reconcil = otherUser                        // system:serviceaccount:koinon:koinon-reconciler
	)
	var (
		admin, rec, ctrl, gw *kubernetes.Clientset
		sandboxSA            *kubernetes.Clientset
	)
	binding := func(mutate ...func(*rbacv1.RoleBinding)) *rbacv1.RoleBinding {
		b := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "koinon-reconciler", Namespace: "koinon"}},
		}
		for _, m := range mutate {
			m(b)
		}
		return b
	}
	create := func(ctx context.Context, c *kubernetes.Clientset, ns string, b *rbacv1.RoleBinding) error {
		_, err := c.RbacV1().RoleBindings(ns).Create(ctx, b, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return err
	}
	denied := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want 403 Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}
	can := func(ctx context.Context, user, ns, verb, group, resource string) bool {
		GinkgoHelper()
		review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User: user, Groups: []string{"system:serviceaccounts", "system:authenticated"},
				ResourceAttributes: &authzv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: group, Resource: resource},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return review.Status.Allowed
	}
	canBind := func(ctx context.Context, name string) bool {
		GinkgoHelper()
		review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User: reconcil, Groups: []string{"system:serviceaccounts", "system:authenticated"},
				ResourceAttributes: &authzv1.ResourceAttributes{Verb: "bind", Group: rbacv1.GroupName, Resource: "clusterroles", Name: name},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return review.Status.Allowed
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		objects := renderObserver(ctx)

		env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{sandboxCRD()}}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		as := func(user string) *kubernetes.Clientset {
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
			return kubernetes.NewForConfigOrDie(c)
		}
		rec, ctrl, gw = as(reconcil), as(controllerUser), as(gatewayUser)

		for name, labels := range map[string]map[string]string{
			managed: {"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell", "openshell.ai/sandbox-workspace": "kw-obs"},
			foreign: {"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "prod", "openshell.ai/sandbox-workspace": "kw-obs"},
			noWs:    {"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell"},
			badName: {"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell", "openshell.ai/sandbox-workspace": "kw-obs"},
			plain:   nil,
		} {
			_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		// kube-system and Koinon's own namespace, labelled like workspace namespaces by someone who means harm.
		for _, name := range []string{"kube-system", "koinon"} {
			var n *corev1.Namespace
			if name == "koinon" {
				n, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
			} else {
				n, err = admin.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			}
			Expect(err).NotTo(HaveOccurred())
			n.Labels = map[string]string{"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell", "openshell.ai/sandbox-workspace": "kw-obs"}
			_, err = admin.CoreV1().Namespaces().Update(ctx, n, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		sandboxSA = as("system:serviceaccount:" + managed + ":default")

		// Only the controller and the gateway may do what the sandbox policy lets them; here they only
		// need RBAC to try to write RoleBindings, so that admission is the one to refuse.
		for _, user := range []string{controllerUser, gatewayUser} {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-obs-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		for _, cr := range objects.roles {
			_, err := admin.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, crb := range objects.roleBindings {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, objects.policy, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, objects.binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("waiting until the policy is enforced")
		Eventually(func() error {
			return create(ctx, gw, managed, binding())
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("lets the reconciler bind the observer role in a workspace namespace of its gateway, with the rendered rights only", func(ctx SpecContext) {
		_, err := rec.RbacV1().RoleBindings(managed).Create(ctx, binding(), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("creating it again is AlreadyExists, which the reconciler counts as done")
		_, err = rec.RbacV1().RoleBindings(managed).Create(ctx, binding(), metav1.CreateOptions{})
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "%v", err)
	})

	It("has the reconciler bind only the observer ClusterRole", func(ctx SpecContext) {
		Expect(canBind(ctx, role)).To(BeTrue())
		for _, other := range []string{"cluster-admin", "admin", "edit", "view", role + "-binder"} {
			Expect(canBind(ctx, other)).To(BeFalse(), "bind %s", other)
		}
		Expect(can(ctx, reconcil, "", "escalate", rbacv1.GroupName, "clusterroles")).To(BeFalse())
		Expect(can(ctx, reconcil, "", "create", rbacv1.GroupName, "clusterrolebindings")).To(BeFalse())
		Expect(can(ctx, reconcil, managed, "delete", rbacv1.GroupName, "rolebindings")).To(BeFalse(), "it creates the binding and never changes or deletes it")
		Expect(can(ctx, reconcil, managed, "update", rbacv1.GroupName, "rolebindings")).To(BeFalse())
	})

	It("denies the reconciler a RoleBinding outside the fence", func(ctx SpecContext) {
		By("in a namespace that is not an OpenShell namespace")
		denied(create(ctx, rec, plain, binding()), "may be bound only in OpenShell workspace namespaces")
		By("in a namespace of another gateway")
		denied(create(ctx, rec, foreign, binding()), "of gateway openshell")
		By("under another name")
		denied(create(ctx, rec, managed, binding(func(b *rbacv1.RoleBinding) { b.Name = "other" })), "must be named "+role)
		By("in kube-system, labelled like a workspace namespace but not named like one")
		denied(create(ctx, rec, "kube-system", binding()), "may be bound only in OpenShell workspace namespaces")
		By("in Koinon's own namespace, labelled the same way")
		denied(create(ctx, rec, "koinon", binding()), "may be bound only in OpenShell workspace namespaces")
		By("in a labelled namespace with a wrong name")
		denied(create(ctx, rec, badName, binding()), "may be bound only in OpenShell workspace namespaces")
		By("in a namespace named right and labelled with the gateway ID but without the sandbox-workspace label")
		denied(create(ctx, rec, noWs, binding()), "may be bound only in OpenShell workspace namespaces")
	})

	Context("with the right to bind any ClusterRole, so that only admission can refuse", func() {
		BeforeAll(func(ctx SpecContext) {
			_, err := admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-obs-bind-any"},
				Rules:      []rbacv1.PolicyRule{{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"clusterroles", "roles"}, Verbs: []string{"bind"}}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-obs-bind-any"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "escape-obs-bind-any"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: reconcil}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return canBind(ctx, "cluster-admin") }).WithContext(ctx).WithTimeout(30 * time.Second).Should(BeTrue())
		})

		It("denies the reconciler another roleRef", func(ctx SpecContext) {
			for _, ref := range []rbacv1.RoleRef{
				{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
				{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role + "-binder"},
				// A namespaced Role that happens to carry the observer's name is not the observer role.
				{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role},
			} {
				denied(create(ctx, rec, managed, binding(func(b *rbacv1.RoleBinding) { b.RoleRef = ref })), "must bind exactly the ClusterRole "+role)
			}
		})

		It("denies the reconciler an extra, a foreign or no subject", func(ctx SpecContext) {
			const want = "must have exactly one subject"
			self := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "koinon-reconciler", Namespace: "koinon"}
			for _, subjects := range map[string][]rbacv1.Subject{
				"an extra user":                 {self, {APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "mallory"}},
				"an extra group":                {self, {APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "system:authenticated"}},
				"the sandbox's own account":     {{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: managed}},
				"another account of koinon":     {{Kind: rbacv1.ServiceAccountKind, Name: "koinon-api", Namespace: "koinon"}},
				"the account in another ns":     {{Kind: rbacv1.ServiceAccountKind, Name: "koinon-reconciler", Namespace: managed}},
				"a user named like the account": {{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "koinon-reconciler", Namespace: "koinon"}},
				"two copies":                    {self, self},
				"none":                          nil,
			} {
				denied(create(ctx, rec, managed, binding(func(b *rbacv1.RoleBinding) { b.Subjects = subjects })), want)
			}
		})

		It("has the reconciler create RoleBindings only: it can neither read nor change one", func(ctx SpecContext) {
			_, err := rec.RbacV1().RoleBindings(managed).Get(ctx, role, metav1.GetOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
			b := binding(func(b *rbacv1.RoleBinding) {
				b.Subjects = append(b.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "mallory"})
			})
			_, err = rec.RbacV1().RoleBindings(managed).Update(ctx, b, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
		})
	})

	It("denies everyone else the observer's name, or its role under any name, in a sandbox namespace, cluster admins included", func(ctx SpecContext) {
		for _, c := range []*kubernetes.Clientset{admin, ctrl, gw} {
			denied(create(ctx, c, managed, binding(func(b *rbacv1.RoleBinding) { b.Name = role + "-dryrun" })), "only the Koinon reconciler may bind the sandbox observer role")
			denied(create(ctx, c, foreign, binding(func(b *rbacv1.RoleBinding) { b.Name = "mine" })), "only the Koinon reconciler may bind the sandbox observer role")
			By("the fixed name with another role")
			denied(create(ctx, c, managed, binding(func(b *rbacv1.RoleBinding) { b.RoleRef.Name = "view" })), "only the Koinon reconciler")
			By("a Role of the observer's name, under another name")
			denied(create(ctx, c, managed, binding(func(b *rbacv1.RoleBinding) { b.Name = "mine"; b.RoleRef.Kind = "Role" })), "only the Koinon reconciler")
		}
		By("and cannot change the reconciler's binding")
		b, err := admin.RbacV1().RoleBindings(managed).Get(ctx, role, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		b.Subjects = append(b.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "mallory"})
		_, err = admin.RbacV1().RoleBindings(managed).Update(ctx, b, metav1.UpdateOptions{})
		denied(err, "only the Koinon reconciler")
	})

	It("leaves other RoleBindings and other namespaces alone (control)", func(ctx SpecContext) {
		unrelated := binding(func(b *rbacv1.RoleBinding) {
			b.Name = "unrelated"
			b.RoleRef.Name = "view"
			b.Subjects = []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "alice"}}
		})
		Expect(create(ctx, admin, managed, unrelated)).To(Succeed())
		Expect(create(ctx, admin, plain, unrelated)).To(Succeed())
		By("the observer's name outside a sandbox namespace is not fenced for others")
		Expect(create(ctx, admin, plain, binding())).To(Succeed())
	})

	It("lets the reconciler read pods and Sandboxes in a bound namespace, and nothing else", func(ctx SpecContext) {
		_, err := admin.CoreV1().Pods(managed).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ka-observed"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "registry.invalid/agent:1"}}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			_, err := rec.CoreV1().Pods(managed).Get(ctx, "ka-observed", metav1.GetOptions{})
			return err
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(can(ctx, reconcil, managed, "get", "agents.x-k8s.io", "sandboxes")).To(BeTrue())
		for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete"} {
			Expect(can(ctx, reconcil, managed, verb, "", "pods")).To(BeFalse(), "%s pods", verb)
			Expect(can(ctx, reconcil, managed, verb, "agents.x-k8s.io", "sandboxes")).To(BeFalse(), "%s sandboxes", verb)
		}
		for _, resource := range []string{"secrets", "events", "configmaps", "pods/exec", "pods/log"} {
			Expect(can(ctx, reconcil, managed, "get", "", resource)).To(BeFalse(), "get %s", resource)
		}
		By("not in a namespace without the binding")
		Expect(can(ctx, reconcil, foreign, "get", "", "pods")).To(BeFalse())
		Expect(can(ctx, reconcil, plain, "get", "", "pods")).To(BeFalse())
		_, err = rec.CoreV1().Pods("kube-system").Get(ctx, "anything", metav1.GetOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
	})

	It("gives a sandbox identity nothing", func(ctx SpecContext) {
		sandbox := "system:serviceaccount:" + managed + ":default"
		for _, resource := range []string{"pods", "secrets"} {
			Expect(can(ctx, sandbox, managed, "get", "", resource)).To(BeFalse(), "get %s", resource)
		}
		Expect(can(ctx, sandbox, managed, "get", "agents.x-k8s.io", "sandboxes")).To(BeFalse())
		_, err := sandboxSA.CoreV1().Pods(managed).Get(ctx, "ka-observed", metav1.GetOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
		By("and cannot bind the role itself")
		_, err = sandboxSA.RbacV1().RoleBindings(managed).Create(ctx, binding(func(b *rbacv1.RoleBinding) {
			b.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: managed}}
		}), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
	})
})

// observerObjects are the objects of templates/sandbox-observer.yaml.
type observerObjects struct {
	roles        []*rbacv1.ClusterRole
	roleBindings []*rbacv1.ClusterRoleBinding
	policy       *admissionv1.ValidatingAdmissionPolicy
	binding      *admissionv1.ValidatingAdmissionPolicyBinding
}

// renderObserver renders the chart's sandbox observer with default values.
func renderObserver(ctx context.Context, extraArgs ...string) observerObjects {
	GinkgoHelper()
	helm, err := exec.LookPath(envOr("HELM", "helm"))
	Expect(err).NotTo(HaveOccurred(), "helm renders the chart's observer")
	var out, stderr bytes.Buffer
	args := append([]string{
		"template", "koinon", "../../charts/koinon", "--namespace", "koinon",
		"--set", "openshell.insecure=true", "--set", "openshell.address=openshell.openshell.svc:8080", "--show-only", "templates/sandbox-observer.yaml",
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, helm, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	Expect(cmd.Run()).To(Succeed(), stderr.String())

	var objs observerObjects
	for _, doc := range strings.Split(out.String(), "\n---") {
		var meta metav1.TypeMeta
		Expect(yaml.Unmarshal([]byte(doc), &meta)).To(Succeed())
		switch meta.Kind {
		case "ClusterRole":
			cr := &rbacv1.ClusterRole{}
			Expect(yaml.UnmarshalStrict([]byte(doc), cr)).To(Succeed())
			objs.roles = append(objs.roles, cr)
		case "ClusterRoleBinding":
			crb := &rbacv1.ClusterRoleBinding{}
			Expect(yaml.UnmarshalStrict([]byte(doc), crb)).To(Succeed())
			objs.roleBindings = append(objs.roleBindings, crb)
		case "ValidatingAdmissionPolicy":
			objs.policy = &admissionv1.ValidatingAdmissionPolicy{}
			Expect(yaml.UnmarshalStrict([]byte(doc), objs.policy)).To(Succeed())
		case "ValidatingAdmissionPolicyBinding":
			objs.binding = &admissionv1.ValidatingAdmissionPolicyBinding{}
			Expect(yaml.UnmarshalStrict([]byte(doc), objs.binding)).To(Succeed())
		case "":
		default:
			Fail("unexpected " + meta.Kind + " in sandbox-observer.yaml")
		}
	}
	Expect(objs.roles).To(HaveLen(2), "the observer and the binder ClusterRoles")
	Expect(objs.roleBindings).To(HaveLen(1))
	Expect(objs.policy).NotTo(BeNil())
	Expect(objs.binding).NotTo(BeNil())
	return objs
}
