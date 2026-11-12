// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The KEK Secret guard and the reconciler Secret fence (issue #604, ADR 0056 amendment 2026-11-11),
// as `helm template` renders templates/kek-guard.yaml with a KMS backend (the key-admin Job, and so
// its writer entry, renders only then; hack/chart-check.d/46-kek-guard.sh asserts the no-KMS
// render leaves it out), against a real API server (envtest). Every identity here holds
// cluster-admin through RBAC, so only the policies decide.
var _ = Describe("KEK Secret guard policies", Label("escape", "admission"), Ordered, ContinueOnFailure, func() {
	const (
		apiUser   = "system:serviceaccount:koinon:koinon-api"
		adminUser = "system:serviceaccount:koinon:koinon-key-admin"
		recUser   = otherUser
		managedBy = "koinon.dev/managed-by"
		ns        = "koinon"
		kek       = "koinon-kek"
	)
	var as map[string]*kubernetes.Clientset

	secret := func(name string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Data:       map[string][]byte{"active": []byte("koinon_test_not-a-real-token")},
		}
	}
	update := func(ctx context.Context, user, name string) error {
		s, err := as["admin"].CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		s.Annotations = map[string]string{"test": user}
		_, err = as[user].CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{})
		return err
	}
	denied := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred(), "admitted, want a denial containing %q", message)
		Expect(err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy"))
		Expect(err.Error()).To(ContainSubstring(message))
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderAdmission(ctx, "koinon", ns, "templates/kek-guard.yaml", nil,
			"--set", "secrets.kek.kms.backend=openbao", "--set", "secrets.kek.kms.openbao.address=https://bao.example:8200",
			"--set", "secrets.kek.kms.openbao.key=koinon-kek")
		Expect(policies).To(HaveLen(2))
		Expect(bindings).To(HaveLen(2))

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin := kubernetes.NewForConfigOrDie(cfg)
		as = map[string]*kubernetes.Clientset{"admin": admin}
		for _, user := range []string{apiUser, adminUser, recUser, "system:serviceaccount:koinon:somebody"} {
			_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "kek-" + strings.ReplaceAll(user, ":", "-")},
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
		for _, n := range []string{kek, "owned", "foreign"} {
			labels := map[string]string{}
			if n == "owned" {
				labels[managedBy] = "koinon-reconciler"
			}
			_, err = admin.CoreV1().Secrets(ns).Create(ctx, secret(n, labels), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, p := range policies {
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, p, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, b := range bindings {
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, b, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("waiting until the policies are enforced")
		Eventually(func() error {
			return update(ctx, recUser, kek)
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("refuses DELETE of the KEK Secret to every identity, cluster-admin included", func(ctx SpecContext) {
		dry := metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
		for _, c := range as {
			denied(c.CoreV1().Secrets(ns).Delete(ctx, kek, dry), "cannot be deleted")
		}
	})

	It("lets koinon-api and the key-admin Job update the KEK Secret, and nobody else", func(ctx SpecContext) {
		Expect(update(ctx, apiUser, kek)).To(Succeed())
		Expect(update(ctx, adminUser, kek)).To(Succeed())
		denied(update(ctx, recUser, kek), "only koinon-api and the key-admin Job may change the KEK Secret")
		denied(update(ctx, "admin", kek), "only koinon-api and the key-admin Job may change the KEK Secret")
		denied(update(ctx, "system:serviceaccount:koinon:somebody", kek), "only koinon-api and the key-admin Job may change the KEK Secret")
	})

	It("leaves other Secrets to RBAC for everyone but the reconciler (control)", func(ctx SpecContext) {
		Expect(update(ctx, apiUser, "foreign")).To(Succeed())
		Expect(update(ctx, "system:serviceaccount:koinon:somebody", "foreign")).To(Succeed())
	})

	It("lets the reconciler write Secrets it owns", func(ctx SpecContext) {
		Expect(update(ctx, recUser, "owned")).To(Succeed())
		_, err := as[recUser].CoreV1().Secrets(ns).Create(ctx, secret("new-owned", map[string]string{managedBy: "koinon-reconciler"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(as[recUser].CoreV1().Secrets(ns).Delete(ctx, "new-owned", metav1.DeleteOptions{})).To(Succeed())
	})

	It("refuses the reconciler every other Secret", func(ctx SpecContext) {
		const message = "may write only Secrets that carry the label"
		denied(update(ctx, recUser, "foreign"), message)
		_, err := as[recUser].CoreV1().Secrets(ns).Create(ctx, secret("new-foreign", nil), metav1.CreateOptions{})
		denied(err, message)
		_, err = as[recUser].CoreV1().Secrets(ns).Create(ctx, secret("new-foreign", map[string]string{managedBy: "koinon-api"}), metav1.CreateOptions{})
		denied(err, message)
		denied(as[recUser].CoreV1().Secrets(ns).Delete(ctx, "foreign", metav1.DeleteOptions{}), message)
		// It cannot take over a Secret by adding the label to it, either.
		s, err := as["admin"].CoreV1().Secrets(ns).Get(ctx, "foreign", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		s.Labels = map[string]string{managedBy: "koinon-reconciler"}
		_, err = as[recUser].CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{})
		denied(err, message)
	})
})
