// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The read-only observer access of the reconciler to the Substrate worker namespace (issue #1162), as
// `helm template` renders templates/substrate-observer.yaml, against a real API server with RBAC
// enforced: the reconciler reads pods and events there, and cannot write, exec, read Secrets or read
// the control-plane namespace.
var _ = Describe("Substrate worker observer access", Label("escape", "admission"), Ordered, func() {
	var admin *kubernetes.Clientset

	can := func(ctx context.Context, ns, verb, group, resource, sub string) bool {
		GinkgoHelper()
		review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User: otherUser, Groups: []string{"system:serviceaccounts", "system:authenticated"},
				ResourceAttributes: &authzv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: group, Resource: resource, Subresource: sub},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return review.Status.Allowed
	}

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Envtest)
		helm, err := exec.LookPath(envOr("HELM", "helm"))
		Expect(err).NotTo(HaveOccurred())
		var out, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, helm, "template", "koinon", "../../charts/koinon", "--namespace", "koinon",
			"--values", "../../charts/koinon/ci/substrate-values.yaml", "--show-only", "templates/substrate-observer.yaml")
		cmd.Stdout, cmd.Stderr = &out, &stderr
		Expect(cmd.Run()).To(Succeed(), stderr.String())

		env := &envtest.Environment{}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		admin = kubernetes.NewForConfigOrDie(cfg)
		for _, ns := range []string{workerNamespace, controlNamespace} {
			_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		var roles, bindings int
		for _, doc := range strings.Split(out.String(), "\n---") {
			var meta metav1.TypeMeta
			Expect(yaml.Unmarshal([]byte(doc), &meta)).To(Succeed())
			switch meta.Kind {
			case "Role":
				r := &rbacv1.Role{}
				Expect(yaml.UnmarshalStrict([]byte(doc), r)).To(Succeed())
				_, err = admin.RbacV1().Roles(r.Namespace).Create(ctx, r, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				roles++
			case "RoleBinding":
				b := &rbacv1.RoleBinding{}
				Expect(yaml.UnmarshalStrict([]byte(doc), b)).To(Succeed())
				_, err = admin.RbacV1().RoleBindings(b.Namespace).Create(ctx, b, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				bindings++
			case "":
			default:
				Fail("unexpected " + meta.Kind + " in substrate-observer.yaml")
			}
		}
		Expect(roles).To(Equal(1))
		Expect(bindings).To(Equal(1))
	})

	It("lets the reconciler read pods, events, workloads and WorkerPools in the worker namespace", func(ctx SpecContext) {
		for _, c := range []struct{ group, resource string }{
			{"", "pods"}, {"", "events"}, {"events.k8s.io", "events"}, {"apps", "deployments"}, {"apps", "replicasets"}, {"ate.dev", "workerpools"},
		} {
			for _, verb := range []string{"get", "list", "watch"} {
				Expect(can(ctx, workerNamespace, verb, c.group, c.resource, "")).To(BeTrue(), "%s %s/%s", verb, c.group, c.resource)
			}
		}
	})

	It("gives the reconciler no write, no pod subresource and no Secret in the worker namespace", func(ctx SpecContext) {
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			for _, c := range []struct{ group, resource string }{
				{"", "pods"}, {"", "events"}, {"apps", "deployments"}, {"ate.dev", "workerpools"}, {"rbac.authorization.k8s.io", "rolebindings"}, {"networking.k8s.io", "networkpolicies"},
			} {
				Expect(can(ctx, workerNamespace, verb, c.group, c.resource, "")).To(BeFalse(), "%s %s/%s", verb, c.group, c.resource)
			}
		}
		for _, sub := range []string{"exec", "attach", "portforward", "proxy", "log", "ephemeralcontainers"} {
			for _, verb := range []string{"get", "create"} {
				Expect(can(ctx, workerNamespace, verb, "", "pods", sub)).To(BeFalse(), "%s pods/%s", verb, sub)
			}
		}
		Expect(can(ctx, workerNamespace, "get", "", "secrets", "")).To(BeFalse())
	})

	It("gives the reconciler nothing in the control-plane namespace", func(ctx SpecContext) {
		Expect(can(ctx, controlNamespace, "get", "", "pods", "")).To(BeFalse())
		Expect(can(ctx, controlNamespace, "list", "", "events", "")).To(BeFalse())
	})
})
