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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The gateway-key guard (ADR 0097 decision 12, issue #1151), as `helm template` renders
// templates/kek-guard.yaml with the MCP driver koinon (the guard renders only with it), against a real API server
// (envtest). The API server compiles the CEL, so a policy that does not compile fails here, not
// only at install. Every identity holds cluster-admin through RBAC, so only the policy decides.
var _ = Describe("gateway key guard policy", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	const (
		ns        = "koinon"
		keySecret = "koinon-gateway-recipient-key"
		adminUser = "system:serviceaccount:koinon:koinon-gateway-key-admin"
		apiUser   = "system:serviceaccount:koinon:koinon-api"
		gwUser    = "system:serviceaccount:koinon:koinon-mcp-gateway"
		message   = "only the gateway key-admin Job may write the Secret " + keySecret
	)
	var as map[string]*kubernetes.Clientset

	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{"keys": []byte("koinon_test_not-a-real-token")},
		}
	}
	update := func(ctx context.Context, user, name string) error {
		s, err := as["admin"].CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		s.Annotations = map[string]string{"test": user}
		_, err = as[user].CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{})
		return err
	}
	denied := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		// Only this policy: the KEK guard and the reconciler fence of the same template have their own specs.
		policies, bindings := renderAdmission(ctx, "koinon", ns, "templates/kek-guard.yaml", nil,
			"--set", "secrets.kek.guard.enabled=false", "--set", "gateway.mcp.driver=koinon")
		var policy *admissionv1.ValidatingAdmissionPolicy
		for _, p := range policies {
			if strings.HasSuffix(p.Name, "-gateway-key-guard") {
				policy = p
			}
		}
		Expect(policy).NotTo(BeNil(), "the render with driver koinon has the gateway key guard")
		Expect(policy.Spec.MatchConstraints.ResourceRules[0].ResourceNames).To(Equal([]string{keySecret}))
		var binding *admissionv1.ValidatingAdmissionPolicyBinding
		for _, b := range bindings {
			if b.Spec.PolicyName == policy.Name {
				binding = b
			}
		}
		Expect(binding).NotTo(BeNil())

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin := kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		for _, user := range []string{adminUser, apiUser, gwUser} {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "gwkey-" + strings.ReplaceAll(user, ":", "-")},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
			as[user] = kubernetes.NewForConfigOrDie(c)
		}
		_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, n := range []string{keySecret, "other"} {
			_, err = admin.CoreV1().Secrets(ns).Create(ctx, secret(n), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		// The API server compiles the policy's CEL on create (#1239: a bare service-account name failed here).
		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("waiting until the policy is enforced")
		Eventually(func() error {
			return update(ctx, apiUser, keySecret)
		}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("lets the gateway key-admin Job update the Secret of the wrapped keys", func(ctx SpecContext) {
		Expect(update(ctx, adminUser, keySecret)).To(Succeed())
	})

	It("refuses every other identity an update or a DELETE, cluster-admin and the gateway included", func(ctx SpecContext) {
		dry := metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
		for _, user := range []string{"admin", apiUser, gwUser} {
			denied(update(ctx, user, keySecret))
			denied(as[user].CoreV1().Secrets(ns).Delete(ctx, keySecret, dry))
		}
	})

	It("leaves other Secrets to RBAC (control)", func(ctx SpecContext) {
		Expect(update(ctx, apiUser, "other")).To(Succeed())
		Expect(update(ctx, gwUser, "other")).To(Succeed())
	})
})
