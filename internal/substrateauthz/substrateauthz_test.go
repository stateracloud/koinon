// SPDX-License-Identifier: Apache-2.0

package substrateauthz_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/substrateauthz"
)

// The principals of a Koinon install in namespace koinon, as Substrate names a ServiceAccount token's caller.
const (
	ns          = "koinon"
	provisioner = "system:serviceaccount:koinon:koinon-provisioner"
	reconciler  = "system:serviceaccount:koinon:koinon-reconciler"
	agentd      = "system:serviceaccount:koinon:koinon-agentd"
	bootstrap   = "system:serviceaccount:ate-system:ate-client"
	org         = "kw-0123456789abcdef"
)

func member(id string) string { return "user:" + id }

var _ = Describe("the Substrate access grants", func() {
	var (
		cl                     *fakeate.Cluster
		ctlProv, ctlRec, ctlAg ateapiv1.ControlClient
		ctlBoot                ateapiv1.ControlClient
		prov                   *substrateauthz.Provisioner
	)

	BeforeEach(func(ctx SpecContext) {
		cl = fakeate.New()
		cl.EnforceAuthz(bootstrap)
		_, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = router.Close(); cl.Stop() })
		as := func(who string) ateapiv1.ControlClient {
			c, err := cl.ControlAs(who)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = c.Close() })
			return ateapiv1.NewControlClient(c)
		}
		ctlBoot, ctlProv, ctlRec, ctlAg = as(bootstrap), as(provisioner), as(reconciler), as(agentd)
		// The installer's step: the provisioner joins the global policy, next to the members that exist.
		cl.SetGlobalPolicy(
			&ateapiv1.Binding{Role: "owner", Members: []string{member("alice")}},
			&ateapiv1.Binding{Role: "viewer", Members: []string{member("bob")}},
		)
		_, err = substrateauthz.EnsureGlobalMember(ctx, ctlBoot, substrateauthz.RoleOwner, member(provisioner))
		Expect(err).NotTo(HaveOccurred())
		prov = &substrateauthz.Provisioner{
			Control: ctlProv,
			Editors: []string{member(reconciler)}, Viewers: []string{member(agentd)},
		}
	})

	Describe("the installer's grant", func() {
		It("adds the provisioner as an owner and keeps the other members", func() {
			p := cl.Policy("")
			Expect(substrateauthz.HasMember(p, "owner", member(provisioner))).To(BeTrue())
			Expect(substrateauthz.HasMember(p, "owner", member("alice"))).To(BeTrue())
			Expect(substrateauthz.HasMember(p, "viewer", member("bob"))).To(BeTrue())
		})

		It("writes nothing on an upgrade that finds the grant", func(ctx SpecContext) {
			before := cl.Calls("UpdateGlobalAccessPolicy")
			changed, err := substrateauthz.EnsureGlobalMember(ctx, ctlBoot, substrateauthz.RoleOwner, member(provisioner))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())
			Expect(cl.Calls("UpdateGlobalAccessPolicy")).To(Equal(before))
		})

		It("creates the policy when there is none", func(ctx SpecContext) {
			fresh := fakeate.New()
			fresh.EnforceAuthz(bootstrap)
			_, router, err := fresh.Start()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = router.Close(); fresh.Stop() })
			c, err := fresh.ControlAs(bootstrap)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = c.Close() })
			changed, err := substrateauthz.EnsureGlobalMember(ctx, ateapiv1.NewControlClient(c), substrateauthz.RoleOwner, member(provisioner))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(substrateauthz.HasMember(fresh.Policy(""), "owner", member(provisioner))).To(BeTrue())
		})

		It("refuses a role that a global policy does not take", func(ctx SpecContext) {
			_, err := substrateauthz.EnsureGlobalMember(ctx, ctlBoot, substrateauthz.RoleEditor, member(provisioner))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("the provisioner", func() {
		It("creates the org atespace with editor for the reconciler and viewer for agentd", func(ctx SpecContext) {
			Expect(prov.EnsureAtespace(ctx, org)).To(Succeed())
			p := cl.Policy(org)
			Expect(substrateauthz.HasMember(p, "editor", member(reconciler))).To(BeTrue())
			Expect(substrateauthz.HasMember(p, "viewer", member(agentd))).To(BeTrue())
			Expect(p.GetBindings()).To(HaveLen(2))
		})

		It("is idempotent and writes the policy once", func(ctx SpecContext) {
			Expect(prov.EnsureAtespace(ctx, org)).To(Succeed())
			Expect(prov.EnsureAtespace(ctx, org)).To(Succeed())
			Expect(cl.Calls("CreateAtespaceAccessPolicy")).To(Equal(1))
			Expect(cl.Calls("UpdateAtespaceAccessPolicy")).To(BeZero())
		})

		It("repairs a policy that drifted and adopts an atespace from before it existed", func(ctx SpecContext) {
			_, err := ctlBoot.CreateAtespace(ctx, &ateapiv1.CreateAtespaceRequest{Atespace: &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: org}}})
			Expect(err).NotTo(HaveOccurred())
			_, err = ctlBoot.CreateAtespaceAccessPolicy(ctx, &ateapiv1.CreateAtespaceAccessPolicyRequest{
				Atespace: &ateapiv1.ObjectRef{Name: org},
				AccessPolicy: &ateapiv1.AccessPolicy{
					Metadata: &ateapiv1.ResourceMetadata{Name: "default"},
					Bindings: []*ateapiv1.Binding{{Role: "editor", Members: []string{member(reconciler), member("mallory")}}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(prov.EnsureAtespace(ctx, org)).To(Succeed())
			p := cl.Policy(org)
			Expect(substrateauthz.HasMember(p, "editor", member("mallory"))).To(BeFalse())
			Expect(substrateauthz.HasMember(p, "viewer", member(agentd))).To(BeTrue())
		})

		It("refuses a name that is not a workspace name", func(ctx SpecContext) {
			for _, bad := range []string{"", "ate-system", "kw-", "kw-0123", "kw-0123456789ABCDEF", "kw-0123456789abcdef0"} {
				Expect(prov.EnsureAtespace(ctx, bad)).To(MatchError(substrateauthz.ErrName), bad)
			}
		})
	})

	Describe("the reconciler", func() {
		BeforeEach(func(ctx SpecContext) { Expect(prov.EnsureAtespace(ctx, org)).To(Succeed()) })

		It("cannot create an atespace or change the global policy", func(ctx SpecContext) {
			_, err := ctlRec.CreateAtespace(ctx, &ateapiv1.CreateAtespaceRequest{Atespace: &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: "kw-fedcba9876543210"}}})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			cur, err := ctlBoot.GetGlobalAccessPolicy(ctx, &ateapiv1.GetGlobalAccessPolicyRequest{Name: "default"})
			Expect(err).NotTo(HaveOccurred())
			cur.Bindings = append(cur.Bindings, &ateapiv1.Binding{Role: "owner", Members: []string{member(reconciler)}})
			_, err = ctlRec.UpdateGlobalAccessPolicy(ctx, &ateapiv1.UpdateGlobalAccessPolicyRequest{AccessPolicy: cur})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			_, err = ctlRec.UpdateAtespaceAccessPolicy(ctx, &ateapiv1.UpdateAtespaceAccessPolicyRequest{Atespace: &ateapiv1.ObjectRef{Name: org}, AccessPolicy: &ateapiv1.AccessPolicy{}})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("works inside its org atespace and in no other", func(ctx SpecContext) {
			_, err := ctlRec.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: &ateapiv1.ObjectRef{Name: org}})
			Expect(err).NotTo(HaveOccurred())
			// A write passes the role check and reaches the handler: the actor is simply not there.
			_, err = ctlRec.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: org, Name: "a"}})
			Expect(status.Code(err)).To(Equal(codes.NotFound))
			_, err = ctlRec.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: "other", Name: "a"}})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})
	})

	Describe("agentd", func() {
		BeforeEach(func(ctx SpecContext) { Expect(prov.EnsureAtespace(ctx, org)).To(Succeed()) })

		It("holds viewer only", func(ctx SpecContext) {
			// Reads pass the check.
			_, err := ctlAg.GetAtespace(ctx, &ateapiv1.GetAtespaceRequest{Atespace: &ateapiv1.ObjectRef{Name: org}})
			Expect(err).NotTo(HaveOccurred())
			_, err = ctlAg.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: org, Name: "a"}})
			Expect(status.Code(err)).To(Equal(codes.NotFound))
			// Everything that changes anything is refused.
			ref := &ateapiv1.ObjectRef{Atespace: org, Name: "a"}
			_, err = ctlAg.SuspendActor(ctx, &ateapiv1.SuspendActorRequest{Actor: ref})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			_, err = ctlAg.ResumeActor(ctx, &ateapiv1.ResumeActorRequest{Actor: ref})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			_, err = ctlAg.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: ref})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			_, err = ctlAg.CreateAtespace(ctx, &ateapiv1.CreateAtespaceRequest{Atespace: &ateapiv1.Atespace{Metadata: &ateapiv1.ResourceMetadata{Name: "kw-fedcba9876543210"}}})
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("is not in the global policy", func() {
			Expect(substrateauthz.HasMember(cl.Policy(""), "owner", member(agentd))).To(BeFalse())
			Expect(substrateauthz.HasMember(cl.Policy(""), "viewer", member(agentd))).To(BeFalse())
		})
	})

	Describe("the provisioner's interface", func() {
		var (
			srv   *httptest.Server
			token string
		)
		tokens := map[string]string{"t-rec": reconciler, "t-agentd": agentd, "t-prov": provisioner}

		BeforeEach(func() {
			srv = httptest.NewServer((&substrateauthz.Server{
				Ensurer: prov, Auth: authFunc(func(_ context.Context, t string) (string, error) {
					if u, ok := tokens[t]; ok {
						return u, nil
					}
					return "", http.ErrNoCookie
				}), Callers: []string{reconciler},
			}).Handler())
			DeferCleanup(srv.Close)
			token = filepath.Join(GinkgoT().TempDir(), "token")
		})
		client := func(tok string) *substrateauthz.Client {
			Expect(os.WriteFile(token, []byte(tok+"\n"), 0o600)).To(Succeed())
			return &substrateauthz.Client{URL: srv.URL, TokenFile: token}
		}

		It("lets the reconciler ask", func(ctx SpecContext) {
			Expect(client("t-rec").EnsureAtespace(ctx, org)).To(Succeed())
			Expect(cl.Policy(org)).NotTo(BeNil())
		})
		It("refuses a valid token of another component", func(ctx SpecContext) {
			Expect(client("t-agentd").EnsureAtespace(ctx, org)).To(MatchError(ContainSubstring("403")))
			Expect(cl.Calls("CreateAtespace")).To(BeZero())
		})
		It("refuses an invalid token", func(ctx SpecContext) {
			Expect(client("nonsense").EnsureAtespace(ctx, org)).To(MatchError(ContainSubstring("401")))
		})
		It("refuses a name that is not a workspace name", func(ctx SpecContext) {
			Expect(client("t-rec").EnsureAtespace(ctx, "ate-system")).To(MatchError(ContainSubstring("400")))
		})

		It("is what the reconciler's driver uses to get its workspace, without creating it", func(ctx SpecContext) {
			conn, err := cl.ControlAs(reconciler)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = conn.Close() })
			d := substrate.New(conn, conn, substrate.Config{Provisioner: client("t-rec")})
			ws, err := d.EnsureWorkspace(ctx, org, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ws.Name).To(Equal(org))
			Expect(ws.Active).To(BeTrue())
			// The one CreateAtespace came from the provisioner's principal: the driver's own principal cannot.
			Expect(cl.Calls("CreateAtespace")).To(Equal(1))
			again, err := d.EnsureWorkspace(ctx, org, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.ID).To(Equal(ws.ID))
			Expect(cl.Calls("CreateAtespace")).To(Equal(1))
		})

		It("fails the workspace with unavailable when the provisioner refuses", func(ctx SpecContext) {
			conn, err := cl.ControlAs(reconciler)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = conn.Close() })
			d := substrate.New(conn, conn, substrate.Config{Provisioner: client("nonsense")})
			_, err = d.EnsureWorkspace(ctx, org, nil)
			Expect(err).To(MatchError(execution.ErrUnavailable))
		})
	})

	Describe("the TokenReview authenticator", func() {
		review := func(authenticated bool, user string, audiences ...string) *fake.Clientset {
			k := fake.NewSimpleClientset()
			k.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
					Authenticated: authenticated, User: authnv1.UserInfo{Username: user}, Audiences: audiences,
				}}, nil
			})
			return k
		}
		It("returns the username of a token for its audience", func(ctx SpecContext) {
			u, err := substrateauthz.TokenReviewer{Kube: review(true, reconciler, "koinon-provisioner"), Audience: "koinon-provisioner"}.Authenticate(ctx, "t")
			Expect(err).NotTo(HaveOccurred())
			Expect(u).To(Equal(reconciler))
		})
		It("refuses a token that is not authenticated or is for another audience", func(ctx SpecContext) {
			_, err := substrateauthz.TokenReviewer{Kube: review(false, ""), Audience: "koinon-provisioner"}.Authenticate(ctx, "t")
			Expect(err).To(HaveOccurred())
			_, err = substrateauthz.TokenReviewer{Kube: review(true, reconciler, "api.ate-system.svc"), Audience: "koinon-provisioner"}.Authenticate(ctx, "t")
			Expect(err).To(HaveOccurred())
		})
	})
})

type authFunc func(context.Context, string) (string, error)

func (f authFunc) Authenticate(ctx context.Context, t string) (string, error) { return f(ctx, t) }
