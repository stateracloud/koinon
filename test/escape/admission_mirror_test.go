// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// With a registry mirror (global.registryMirrors / global.imageRegistry), the chart rewrites the
// default sandboxPolicy.supervisorImages with it (old issue 130), so a supervisor pod that OpenShell pulls
// from the mirror is still admitted and the upstream reference is not. Applied to a real API
// server like the policies in admission_test.go.
var _ = Describe("Sandbox admission policy with a registry mirror", Label("escape", "admission"), Ordered, func() {
	const (
		ns       = "kw-mirror"
		upstream = "ghcr.io/nvidia/openshell/supervisor:0.1.7"
		mirrored = "art.example.com/ghcr-remote/nvidia/openshell/supervisor:0.1.7"
	)
	var gateway *kubernetes.Clientset
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	mirrorSupervisor := func(name, image string) *corev1.Pod {
		return supervisorPod(name, func(p *corev1.Pod) { p.Spec.Containers[0].Image = image })
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		policies, bindings := renderSandboxPolicy(ctx, "--set", "global.registryMirrors.ghcr\\.io=art.example.com/ghcr-remote")

		env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{sandboxCRD()}}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin := kubernetes.NewForConfigOrDie(cfg)

		_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "escape-mirror-gateway"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: gatewayUser}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: gatewayUser, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
		gateway = kubernetes.NewForConfigOrDie(c)

		_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"openshell.ai/managed-by": "openshell", "openshell.ai/gateway-id": "openshell"}},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// No controller manager in envtest: the ServiceAccount admission plugin needs it.
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
			_, err := gateway.CoreV1().Pods(ns).Create(ctx, mirrorSupervisor("probe", "registry.invalid/koinon-escape/evil:1"), dryRun)
			return err
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(MatchError(ContainSubstring("ValidatingAdmissionPolicy")))
	}, NodeTimeout(3*time.Minute))

	It("admits the supervisor image from the mirror and denies the upstream reference", func(ctx SpecContext) {
		_, err := gateway.CoreV1().Pods(ns).Create(ctx, mirrorSupervisor("sup-mirrored", mirrored), dryRun)
		Expect(err).NotTo(HaveOccurred())

		_, err = gateway.CoreV1().Pods(ns).Create(ctx, mirrorSupervisor("sup-upstream", upstream), dryRun)
		Expect(err).To(MatchError(ContainSubstring("must use the pinned supervisor image (" + mirrored + ")")))
	})
})
