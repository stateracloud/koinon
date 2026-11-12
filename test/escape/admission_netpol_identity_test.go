// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// NetworkPolicies are additive allows: in a sandbox namespace, any identity that may create or
// change one can open the sandbox's egress, and an identity that may delete the default-deny
// removes the floor under OpenShell's own policies (issue 673, ADR 0045). The writer policy of
// templates/sandbox-networkpolicy.yaml, rendered with default values, against envtest: in this
// gateway's sandbox namespaces only the reconciler writes or deletes the default-deny, only the
// OpenShell gateway changes any other NetworkPolicy, and a namespace being deleted is emptied
// without a denial.
var _ = Describe("Sandbox NetworkPolicy identity rule", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	const (
		reconcilerUser = otherUser // system:serviceaccount:koinon:koinon-reconciler, release koinon in koinon
		name           = "koinon-koinon-sandbox-default-deny"
		openshellOwn   = "openshell-sandbox-workloads" // a namespace policy OpenShell 0.1.7 writes (isolation.rs)
	)
	var (
		admin      *kubernetes.Clientset
		as         map[string]*kubernetes.Clientset
		ws         = "openshell-openshell-kw-npid" // a workspace namespace of this release's gateway
		wsB        = "openshell-prod-kw-npid"      // a workspace namespace of another gateway (ID prod)
		unlabelled = "openshell-kw-npid-nogw"      // managed by OpenShell, no gateway-id: no gateway owns it
		gone       = "openshell-openshell-kw-gone" // a workspace namespace being deleted
		plain      = "koinon-npid-plain"           // not a sandbox namespace
		dryCreate  = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
		dryUpdate  = metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}
		dryDelete  = metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
	)
	defaultDeny := func() *networkingv1.NetworkPolicy {
		return (&reconciler.SandboxNetworkPolicy{Name: name}).Policy("")
	}
	// allowAll is a policy that opens every pod's egress: additive with the default-deny.
	allowAll := func(n string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: n},
			Spec: networkingv1.NetworkPolicySpec{
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
			},
		}
	}
	denied := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}
	create := func(ctx context.Context, user, namespace string, np *networkingv1.NetworkPolicy, opts metav1.CreateOptions) error {
		_, err := as[user].NetworkingV1().NetworkPolicies(namespace).Create(ctx, np, opts)
		return err
	}
	del := func(ctx context.Context, user, namespace, n string) error {
		return as[user].NetworkingV1().NetworkPolicies(namespace).Delete(ctx, n, dryDelete)
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderAdmission(ctx, "koinon", "koinon", "templates/sandbox-networkpolicy.yaml", []string{"ClusterRole", "ClusterRoleBinding"})
		Expect(policies).To(HaveLen(1))
		Expect(bindings).To(HaveLen(1))

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		for _, user := range []string{reconcilerUser, gatewayUser, gatewayBUser} {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "npid-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
			as[user] = kubernetes.NewForConfigOrDie(c)
		}
		workspace := func(gateway string) map[string]string {
			return map[string]string{"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": gateway, "openshell.ai/sandbox-workspace": "kw-npid"}
		}
		for n, labels := range map[string]map[string]string{
			ws:         workspace("openshell"),
			wsB:        workspace("prod"),
			gone:       workspace("openshell"),
			unlabelled: {"openshell.ai/managed-by": "openshell"},
			plain:      nil,
		} {
			_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: labels}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("the policies each namespace holds before the policy is bound")
		// Created before the binding so that the fixtures do not depend on the rules under test.
		for _, n := range []string{ws, gone} {
			Expect(create(ctx, "admin", n, defaultDeny(), metav1.CreateOptions{})).To(Succeed())
			Expect(create(ctx, "admin", n, allowAll(openshellOwn), metav1.CreateOptions{})).To(Succeed())
		}
		Expect(create(ctx, "admin", wsB, allowAll(openshellOwn), metav1.CreateOptions{})).To(Succeed())

		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policies[0], metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, bindings[0], metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("waiting until the policy is enforced")
		Eventually(func() error {
			return create(ctx, "admin", ws, defaultDeny(), dryCreate)
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))

		By("deleting one workspace namespace: with no namespace controller in envtest it stays Terminating")
		Expect(admin.CoreV1().Namespaces().Delete(ctx, gone, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			n, err := admin.CoreV1().Namespaces().Get(ctx, gone, metav1.GetOptions{})
			return err == nil && n.DeletionTimestamp != nil
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
	}, NodeTimeout(3*time.Minute))

	It("denies deleting the default-deny in a workspace namespace to every identity but the reconciler", func(ctx SpecContext) {
		const msg = "only the Koinon reconciler may write or delete the sandbox default-deny NetworkPolicy"
		for _, user := range []string{"admin", gatewayUser, gatewayBUser} {
			By(user)
			denied(del(ctx, user, ws, name), msg)
		}
		By("the reconciler (control)")
		Expect(del(ctx, reconcilerUser, ws, name)).To(Succeed())
	})

	It("denies creating any other NetworkPolicy in this gateway's sandbox namespaces to every identity but the OpenShell gateway", func(ctx SpecContext) {
		const msg = "only the OpenShell gateway may write or delete NetworkPolicies in sandbox namespaces"
		for _, n := range []string{ws, unlabelled} {
			for _, user := range []string{"admin", gatewayBUser} {
				By(n + " as " + user)
				denied(create(ctx, user, n, allowAll("open"), dryCreate), msg)
			}
			By(n + " as the gateway (control)")
			Expect(create(ctx, gatewayUser, n, allowAll("openshell-own"), dryCreate)).To(Succeed())
		}
		By("the reconciler, under another name")
		denied(create(ctx, reconcilerUser, ws, allowAll("open"), dryCreate), "must be named "+name)
	})

	It("denies changing or deleting OpenShell's NetworkPolicies to every identity but the OpenShell gateway", func(ctx SpecContext) {
		const msg = "only the OpenShell gateway may write or delete NetworkPolicies in sandbox namespaces"
		cur, err := admin.NetworkingV1().NetworkPolicies(ws).Get(ctx, openshellOwn, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		widened := cur.DeepCopy()
		widened.Spec.PodSelector = metav1.LabelSelector{}
		_, err = admin.NetworkingV1().NetworkPolicies(ws).Update(ctx, widened, dryUpdate)
		denied(err, msg)
		denied(del(ctx, "admin", ws, openshellOwn), msg)

		By("the gateway (control)")
		_, err = as[gatewayUser].NetworkingV1().NetworkPolicies(ws).Update(ctx, widened, dryUpdate)
		Expect(err).NotTo(HaveOccurred())
		Expect(del(ctx, gatewayUser, ws, openshellOwn)).To(Succeed())

		By("a metadata-only update by another identity (control: finalizers, owner references, labels)")
		labelled := cur.DeepCopy()
		labelled.Labels = map[string]string{"koinon.dev/escape": "1"}
		_, err = admin.NetworkingV1().NetworkPolicies(ws).Update(ctx, labelled, dryUpdate)
		Expect(err).NotTo(HaveOccurred())
	})

	It("lets a namespace that is being deleted be emptied (control)", func(ctx SpecContext) {
		Expect(del(ctx, "admin", gone, name)).To(Succeed())
		Expect(del(ctx, "admin", gone, openshellOwn)).To(Succeed())
	})

	It("leaves other gateways' and plain namespaces to RBAC (control)", func(ctx SpecContext) {
		Expect(create(ctx, gatewayBUser, wsB, allowAll("prod-own"), dryCreate)).To(Succeed())
		Expect(del(ctx, gatewayBUser, wsB, openshellOwn)).To(Succeed())
		Expect(create(ctx, "admin", plain, allowAll("open"), dryCreate)).To(Succeed())
		Expect(create(ctx, "admin", plain, defaultDeny(), dryCreate)).To(Succeed())
	})
})
