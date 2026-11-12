// SPDX-License-Identifier: Apache-2.0

package recovery_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	admissionv1 "k8s.io/api/admissionregistration/v1"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/recovery"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tokens"
)

const (
	ns       = "koinon"
	policy   = "koinon-koinon-platform-recovery"
	fakeNon1 = "koinon_test_nonce_0000000001"
	fakeNon2 = "koinon_test_nonce_0000000002"
)

// open decrypts an armored age result with the requester's private key.
func open(id *age.X25519Identity, sealed string) map[string]any {
	GinkgoHelper()
	r, err := age.Decrypt(armor.NewReader(strings.NewReader(sealed)), id)
	Expect(err).NotTo(HaveOccurred())
	b, err := io.ReadAll(r)
	Expect(err).NotTo(HaveOccurred())
	var m map[string]any
	Expect(json.Unmarshal(b, &m)).To(Succeed())
	return m
}

var _ = Describe("the PlatformAdminRecovery request (issue #512, ADR 0110 decision 6)", func() {
	var (
		pool *pgxpool.Pool
		id   *age.X25519Identity
		h    *recovery.Handler
		now  time.Time
	)

	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		pw := filepath.Join(GinkgoT().TempDir(), "password")
		Expect(os.WriteFile(pw, []byte(fakeOldPassword+"\n"), 0o600)).To(Succeed())
		_, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{Username: "admin", PasswordFile: pw, LoginEnabled: true}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		id, err = age.GenerateX25519Identity()
		Expect(err).NotTo(HaveOccurred())
		now = time.Now()
		h = &recovery.Handler{Pool: pool, Tokens: tokens.New(pool, nil), Now: func() time.Time { return now }}
	})

	req := func(action, nonce string) recovery.Request {
		return recovery.Request{
			Name: "r1", UID: "uid-" + nonce, Created: now.Add(-time.Minute), Action: action, Requester: "alice", Groups: []string{"ops"},
			Nonce: nonce, RecipientPublicKey: id.Recipient().String(),
		}
	}

	It("resets the bootstrap admin's password, seals it to the requester and audits the request", func(ctx SpecContext) {
		o, err := h.Handle(ctx, req(recovery.ActionResetPassword, fakeNon1))
		Expect(err).NotTo(HaveOccurred())
		Expect(o.Done).To(BeTrue(), o.Reason)
		Expect(o.Sealed).NotTo(ContainSubstring(fakeOldPassword))
		res := open(id, o.Sealed)
		Expect(res).To(HaveKeyWithValue("username", "admin"))
		Expect(res["password"]).NotTo(BeEmpty())

		var must bool
		Expect(pool.QueryRow(ctx, `SELECT must_change_password FROM local_credentials WHERE username = 'admin'`).Scan(&must)).To(Succeed())
		Expect(must).To(BeTrue())
		var actor, summary string
		Expect(pool.QueryRow(ctx, `SELECT actor_id, summary::text FROM install_audit_events WHERE action = 'recovery.reset-password'`).Scan(&actor, &summary)).To(Succeed())
		Expect(actor).To(Equal("kubernetes:alice"))
		Expect(summary).To(ContainSubstring(fakeNon1))
		Expect(summary).NotTo(ContainSubstring(res["password"].(string)))
	})

	It("mints an admin token for the bootstrap admin and seals it", func(ctx SpecContext) {
		o, err := h.Handle(ctx, req(recovery.ActionMintAdminToken, fakeNon1))
		Expect(err).NotTo(HaveOccurred())
		Expect(o.Done).To(BeTrue(), o.Reason)
		res := open(id, o.Sealed)
		Expect(res["token"]).To(HavePrefix(tokens.Prefix))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'recovery.mint-admin-token'`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("does nothing the second time: a re-applied manifest and a retry of the same object", func(ctx SpecContext) {
		first, err := h.Handle(ctx, req(recovery.ActionResetPassword, fakeNon1))
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Done).To(BeTrue())

		// The same object again (its status write had failed): the stored result, no second reset.
		again, err := h.Handle(ctx, req(recovery.ActionResetPassword, fakeNon1))
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(first))

		// A new object with the same nonce (a GitOps sync, a copy-paste): refused.
		other := req(recovery.ActionResetPassword, fakeNon1)
		other.UID = "another-uid"
		o, err := h.Handle(ctx, other)
		Expect(err).NotTo(HaveOccurred())
		Expect(o.Done).To(BeFalse())
		Expect(o.Reason).To(ContainSubstring("nonce"))

		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'recovery.reset-password'`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	DescribeTable("refuses and changes nothing",
		func(ctx SpecContext, mut func(*recovery.Request), want string) {
			r := req(recovery.ActionResetPassword, fakeNon2)
			mut(&r)
			o, err := h.Handle(ctx, r)
			Expect(err).NotTo(HaveOccurred())
			Expect(o.Done).To(BeFalse())
			Expect(o.Reason).To(ContainSubstring(want))
			Expect(o.Sealed).To(BeEmpty())
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM recovery_requests`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), "a refusal burns no nonce")
		},
		Entry("an expired request", func(r *recovery.Request) { r.Created = now.Add(-20 * time.Minute) }, "expired"),
		Entry("a ttl over an hour", func(r *recovery.Request) { r.TTL = 2 * time.Hour }, "ttl"),
		Entry("an unknown action", func(r *recovery.Request) { r.Action = "adopt-restore" }, "not supported"),
		Entry("a short nonce", func(r *recovery.Request) { r.Nonce = "short" }, "nonce"),
		Entry("no recipient key", func(r *recovery.Request) { r.RecipientPublicKey = "" }, "recipientPublicKey"),
		Entry("a malformed recipient key", func(r *recovery.Request) { r.RecipientPublicKey = "age1nope" }, "recipientPublicKey"),
		Entry("an unknown user", func(r *recovery.Request) { r.User = "nobody" }, "no local user"),
	)

	It("refuses a token for a user who is not a platform admin", func(ctx SpecContext) {
		// A second local user that is not a platform admin.
		_, err := pool.Exec(ctx, `WITH u AS (INSERT INTO users (issuer, subject, email, display_name, personal_org_id)
				SELECT 'koinon:local', 'plain', 'plain@example.invalid', 'Plain', personal_org_id FROM users WHERE subject = 'admin' RETURNING id)
			INSERT INTO local_credentials (user_id, username, password_hash) SELECT id, 'plain', 'x' FROM u`)
		if err != nil {
			Skip("fixture for a second local user does not fit the schema: " + err.Error())
		}
		r := req(recovery.ActionMintAdminToken, fakeNon2)
		r.User = "plain"
		o, err := h.Handle(ctx, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(o.Done).To(BeFalse())
		Expect(o.Reason).To(ContainSubstring("platform admin"))
	})
})

// The controller needs no database for these: every refusal happens before the handler.
var _ = Describe("the PlatformAdminRecovery controller", func() {
	var (
		dyn     *dynfake.FakeDynamicClient
		kube    *kubefake.Clientset
		c       *recovery.Controller
		created time.Time
	)
	ctxb := context.Background()

	object := func(name, uid string, spec map[string]any, status map[string]any) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": recovery.Group + "/" + recovery.Version, "kind": recovery.Kind,
			"metadata": map[string]any{"name": name, "namespace": ns},
			"spec":     spec,
		}}
		o.SetUID(types.UID(uid))
		o.SetCreationTimestamp(metav1.NewTime(created))
		if status != nil {
			o.Object["status"] = status
		}
		return o
	}
	spec := map[string]any{"action": "reset-password", "requester": "alice", "nonce": fakeNon1, "recipientPublicKey": "age1x"}

	failClosed := admissionv1.Fail
	good := func() (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
		return &admissionv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policy}, Spec: admissionv1.ValidatingAdmissionPolicySpec{FailurePolicy: &failClosed},
		}, &admissionv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: policy, CreationTimestamp: metav1.NewTime(created.Add(-time.Hour))},
			Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName: policy, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
				MatchResources: &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}},
			},
		}
	}
	build := func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding, objs ...runtime.Object) {
		var kobjs []runtime.Object
		if p != nil {
			kobjs = append(kobjs, p)
		}
		if b != nil {
			kobjs = append(kobjs, b)
		}
		kube = kubefake.NewSimpleClientset(kobjs...)
		dyn = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{recovery.GVR: "PlatformAdminRecoveryList"}, objs...)
		c = &recovery.Controller{
			Dynamic: dyn, Kube: kube, Namespace: ns, Policy: policy, Binding: policy, Handler: &recovery.Handler{},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return created.Add(time.Minute) },
		}
	}
	status := func(name string) map[string]any {
		GinkgoHelper()
		o, err := dyn.Resource(recovery.GVR).Namespace(ns).Get(ctxb, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		s, _, _ := unstructured.NestedMap(o.Object, "status")
		return s
	}

	BeforeEach(func() { created = time.Now().Add(-time.Minute) })

	DescribeTable("refuses while the admission policy is not in force",
		func(mut func(*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding), reason string) {
			p, b := good()
			mut(p, b)
			build(p, b, object("r1", "u1", spec, nil))
			c.Pass(ctxb)
			s := status("r1")
			Expect(s).To(HaveKeyWithValue("phase", "Refused"))
			Expect(s["reason"]).To(ContainSubstring(reason))
		},
		Entry("failurePolicy Ignore", func(p *admissionv1.ValidatingAdmissionPolicy, _ *admissionv1.ValidatingAdmissionPolicyBinding) {
			ig := admissionv1.Ignore
			p.Spec.FailurePolicy = &ig
		}, "failurePolicy"),
		Entry("a Warn binding", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Deny, admissionv1.Warn}
		}, "validationActions"),
		Entry("another policy name", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.PolicyName = "other"
		}, "names policy"),
		Entry("another namespace", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] = "elsewhere"
		}, "release namespace"),
		Entry("an excluded resource", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources.ExcludeResourceRules = []admissionv1.NamedRuleWithOperations{{}}
		}, "excludeResourceRules"),
		Entry("an object selector", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
		}, "objectSelector"),
		Entry("a binding made after the request", func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.CreationTimestamp = metav1.NewTime(created.Add(time.Hour))
		}, "older than the binding"),
	)

	It("refuses when the policy or the binding is missing", func() {
		_, b := good()
		build(nil, b, object("r1", "u1", spec, nil))
		c.Pass(ctxb)
		Expect(status("r1")["reason"]).To(ContainSubstring("not in force"))
	})

	It("leaves a handled object alone until its ttl passes, then deletes it", func() {
		p, b := good()
		done := map[string]any{"phase": "Done", "sealedResult": "x"}
		build(p, b, object("r1", "u1", spec, done))
		c.Pass(ctxb)
		Expect(status("r1")).To(HaveKeyWithValue("phase", "Done"))
		c.Now = func() time.Time { return created.Add(16 * time.Minute) }
		c.Pass(ctxb)
		_, err := dyn.Resource(recovery.GVR).Namespace(ns).Get(ctxb, "r1", metav1.GetOptions{})
		Expect(err).To(HaveOccurred())
	})

	It("writes a refusal for an unparsable ttl", func() {
		p, b := good()
		bad := map[string]any{"action": "reset-password", "requester": "alice", "nonce": fakeNon1, "ttl": "soon"}
		build(p, b, object("r1", "u1", bad, nil))
		c.Pass(ctxb)
		Expect(status("r1")["reason"]).To(ContainSubstring("spec.ttl"))
	})

	It("parses the spec", func() {
		o := object("r1", "u1", map[string]any{
			"action": "mint-admin-token", "requester": "alice", "requesterGroups": []any{"ops", "dev"}, "nonce": fakeNon1,
			"ttl": "10m", "tokenTTL": "2h", "user": "admin", "org": "default", "recipientPublicKey": "age1x",
		}, nil)
		r, err := recovery.ParseRequest(o)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Groups).To(Equal([]string{"ops", "dev"}))
		Expect(r.TTL).To(Equal(10 * time.Minute))
		Expect(r.TokenTTL).To(Equal(2 * time.Hour))
		Expect(r.UID).To(Equal("u1"))
	})
})
