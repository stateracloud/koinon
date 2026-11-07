// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// Remote MCP servers (plan item MC1): the reconciler renders each org's servers as gateway targets
// visible to that org only, delivers their credentials as files, and grants each agent the tools of
// the servers it is allowed by tier.
var _ = Describe("Org MCP servers", func() {
	const secretName = "koinon-gateway-providers"
	var (
		mg   *fakeModelGateway
		kube *fake.Clientset
		r    *reconciler.Reconciler
		svc  *mcpservers.Service
		org  pgtype.UUID
		user pgtype.UUID
		slug string
	)

	BeforeEach(func(ctx SpecContext) {
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		mg = &fakeModelGateway{}
		kube = fake.NewClientset()
		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(testPolicy)
		r, err = reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
			ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy,
			MCPAskURL: "http://koinon-api-tools.koinon.svc:8081/mcp-ask",
			Flavors: map[store.AgentFlavor]reconciler.Flavor{
				store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			},
			Logger: slog.New(slog.DiscardHandler),
		})
		Expect(err).NotTo(HaveOccurred())
		org, user = newOrg(ctx)
		Expect(pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, org).Scan(&slug)).To(Succeed())
	})

	// server adds a server with tools in the given tiers.
	server := func(ctx SpecContext, org pgtype.UUID, name, secret string, tiers map[string]string) mcpservers.Server {
		GinkgoHelper()
		actor := audit.System("koinon-test:spec")
		s, err := svc.Create(ctx, org, actor, user, mcpservers.Input{
			Name: name, URL: "https://mcp.example.com/" + name, CredentialHeader: "X-Api-Key", CredentialValue: secret,
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = svc.Delete(ctx, org, audit.System("koinon-test:spec"), s.ID) })
		var listed []mcpservers.ListedTool
		for tool := range tiers {
			listed = append(listed, mcpservers.ListedTool{Name: tool, Description: tool, InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		var id pgtype.UUID
		Expect(id.Scan(s.ID)).To(Succeed())
		_, err = svc.SyncTools(ctx, org, id, listed)
		Expect(err).NotTo(HaveOccurred())
		for tool, tier := range tiers {
			_, err := svc.SetTier(ctx, org, actor, s.ID, tool, tier)
			Expect(err).NotTo(HaveOccurred())
		}
		return s
	}
	allow := func(ctx SpecContext, agent store.Agent, srv mcpservers.Server, tools []string) {
		GinkgoHelper()
		var sid pgtype.UUID
		Expect(sid.Scan(srv.ID)).To(Succeed())
		Expect(q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: agent.OrgID, AgentID: agent.ID, ServerID: sid, Tools: tools})).To(Succeed())
	}
	targetOf := func(st gateway.State, name string) (gateway.MCPTarget, bool) {
		for _, t := range st.MCP {
			if t.Name == name {
				return t, true
			}
		}
		return gateway.MCPTarget{}, false
	}
	ref := func(slug string, a store.Agent) gateway.AgentRef {
		return gateway.AgentRef{Org: slug, Name: a.ID.String()}
	}

	It("renders an org-scoped target with its credential as a file, never the secret", func(ctx SpecContext) {
		s := server(ctx, org, "wiki", "not-a-real-token", map[string]string{"read": "auto"})

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		t, ok := targetOf(st, slug+"-wiki")
		Expect(ok).To(BeTrue())
		Expect(t.Org).To(Equal(slug))
		Expect(t.URL).To(Equal("https://mcp.example.com/wiki"))
		Expect(t.CredentialHeader).To(Equal("X-Api-Key"))
		Expect(t.CredentialFile).To(HavePrefix("/secrets/providers/mcp-" + s.ID + "-"))
		Expect(fmt.Sprintf("%+v", st)).NotTo(ContainSubstring("not-a-real-token"))

		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(sec.Data[strings.TrimPrefix(t.CredentialFile, "/secrets/providers/")])).To(Equal("not-a-real-token"))
	})

	It("grants an agent the auto tools directly, the ask tools through the approval proxy, and no never tools", func(ctx SpecContext) {
		s := server(ctx, org, "wiki", "", map[string]string{"read": "auto", "list": "auto", "write": "ask", "drop": "never"})
		a := newAgent(ctx, org, user)
		b := newAgent(ctx, org, user)
		allow(ctx, a, s, nil)
		allow(ctx, b, s, []string{"read", "write"})

		Expect(r.ReconcileAll(ctx)).To(Succeed())

		st := mg.last()
		Expect(st.Tools[ref(slug, a)]).To(ConsistOf(
			gateway.ToolGrant{Target: slug + "-wiki", Tools: []string{"list", "read"}},
			gateway.ToolGrant{Target: slug + "-wiki-ask", Tools: []string{"write", "get_approval_status"}},
		))
		Expect(st.Tools[ref(slug, b)]).To(ConsistOf(
			gateway.ToolGrant{Target: slug + "-wiki", Tools: []string{"read"}},
			gateway.ToolGrant{Target: slug + "-wiki-ask", Tools: []string{"write", "get_approval_status"}},
		))
		ask, ok := targetOf(st, slug+"-wiki-ask")
		Expect(ok).To(BeTrue())
		Expect(ask.URL).To(Equal("http://koinon-api-tools.koinon.svc:8081/mcp-ask/wiki"))
		Expect(ask.Org).To(Equal(slug))

		By("an agent without an allowance sees nothing, and neither does another org's agent")
		c := newAgent(ctx, org, user)
		otherOrg, otherUser := newOrg(ctx)
		other := newAgent(ctx, otherOrg, otherUser)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		st = mg.last()
		Expect(st.Tools).NotTo(HaveKey(ref(slug, c)))
		for ag := range st.Tools {
			if ag.Name == other.ID.String() {
				Fail("another org's agent got tools")
			}
		}
	})

	It("keeps an unhealthy server's target but withdraws its tools, and restores them on recovery", func(ctx SpecContext) {
		s := server(ctx, org, "wiki", "", map[string]string{"read": "auto"})
		var sid pgtype.UUID
		Expect(sid.Scan(s.ID)).To(Succeed())
		a := newAgent(ctx, org, user)
		allow(ctx, a, s, nil)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Tools).To(HaveKey(ref(slug, a)))

		for range mcpservers.FailureThreshold {
			_, _, err := svc.RecordProbe(ctx, org, sid, false, "connection refused", false)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		st := mg.last()
		Expect(st.Tools).NotTo(HaveKey(ref(slug, a)))
		_, ok := targetOf(st, slug+"-wiki")
		Expect(ok).To(BeTrue(), "the target stays so the prober can see it recover")

		_, _, err := svc.RecordProbe(ctx, org, sid, true, "", true)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Tools).To(HaveKey(ref(slug, a)))
	})

	It("gives each served server a probe key that lists its tools and nothing else", func(ctx SpecContext) {
		server(ctx, org, "wiki", "", map[string]string{"read": "auto"})
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		st := mg.last()
		probe := gateway.AgentRef{Org: slug, Name: "mcp-probe-wiki"}
		Expect(st.Tools[probe]).To(Equal([]gateway.ToolGrant{{Target: slug + "-wiki", All: true}}))
		var found bool
		for _, k := range st.Keys {
			found = found || k.Agent == probe
		}
		Expect(found).To(BeTrue())
		Expect(st.Validate()).To(Succeed())
	})

	Describe("the prober", func() {
		// fakeGateway answers like agentgateway for one target: tools prefixed with the target's name.
		fakeGateway := func(target string, tools ...string) (host string, port int, stop func()) {
			srv := mcp.NewServer(&mcp.Implementation{Name: "fake-gateway", Version: "0"}, nil)
			for _, t := range tools {
				srv.AddTool(&mcp.Tool{
					Name: target + "_" + t, Description: "does " + t, InputSchema: json.RawMessage(`{"type":"object"}`),
					Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t == "read"},
				},
					func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return &mcp.CallToolResult{}, nil
					})
			}
			h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
			ts := httptest.NewServer(h)
			u, _ := url.Parse(ts.URL)
			p, _ := strconv.Atoi(u.Port())
			return u.Hostname(), p, func() { ts.CloseClientConnections(); go ts.Close() }
		}
		probing := func(ctx SpecContext, host string, port int) *reconciler.Reconciler {
			osg := fakegateway.New()
			cc, err := osg.Start()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
			rr, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
				Prices:  prices.NewResolver(pricing.Embedded()),
				Gateway: reconciler.Endpoint{Host: host, Port: uint32(port)}, Keys: ring,
				ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: secretName},
				ProviderKeyDir: "/secrets/providers", URLPolicy: testPolicy, Logger: slog.New(slog.DiscardHandler),
			})
			Expect(err).NotTo(HaveOccurred())
			return rr
		}
		tiers := func(ctx SpecContext, s mcpservers.Server) map[string]mcpservers.Tool {
			GinkgoHelper()
			ts, err := svc.Tools(ctx, org, s.ID)
			Expect(err).NotTo(HaveOccurred())
			out := map[string]mcpservers.Tool{}
			for _, t := range ts {
				out[t.Name] = t
			}
			return out
		}

		It("syncs the tools a server lists into the ask tier and marks the server healthy", func(ctx SpecContext) {
			s := server(ctx, org, "wiki", "", nil)
			host, port, stop := fakeGateway(slug+"-wiki", "read", "write")
			DeferCleanup(stop)
			rr := probing(ctx, host, port)
			Expect(rr.ReconcileAll(ctx)).To(Succeed())
			rr.ProbeAll(ctx)

			got := tiers(ctx, s)
			Expect(got).To(HaveKey("read"))
			Expect(got).To(HaveKey("write"))
			Expect(got["read"].Tier).To(Equal("ask"))
			Expect(got["read"].ReadOnlyHint).To(BeTrue())
			Expect(got["write"].ReadOnlyHint).To(BeFalse())
			after, err := svc.Get(ctx, org, s.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Health).To(Equal("healthy"))
			Expect(after.ToolsSyncedAt).NotTo(BeNil())
		})

		It("probes a newly served server right after the pass that applied it, not at the next tick (koinon-private#226)", func(ctx SpecContext) {
			s := server(ctx, org, "wiki", "", nil)
			host, port, stop := fakeGateway(slug+"-wiki", "read")
			DeferCleanup(stop)
			rr := probing(ctx, host, port)
			pctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); rr.RunProber(pctx) }() // the default tick is a minute
			DeferCleanup(func() { cancel(); <-done })

			Expect(rr.ReconcileAll(ctx)).To(Succeed())
			Eventually(func(g Gomega) {
				got, err := svc.Get(ctx, org, s.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got.Health).To(Equal("healthy"))
			}).WithTimeout(10 * time.Second).Should(Succeed())
			Expect(tiers(ctx, s)).To(HaveKey("read"))
		})

		It("turns a server unhealthy after three probes with no tools, and says why", func(ctx SpecContext) {
			s := server(ctx, org, "wiki", "", nil)
			host, port, stop := fakeGateway(slug + "-wiki") // lists nothing: the gateway skipped an unreachable target
			DeferCleanup(stop)
			rr := probing(ctx, host, port)
			Expect(rr.ReconcileAll(ctx)).To(Succeed())
			for i := range mcpservers.FailureThreshold {
				rr.ProbeAll(ctx)
				got, err := svc.Get(ctx, org, s.ID)
				Expect(err).NotTo(HaveOccurred())
				if i < mcpservers.FailureThreshold-1 {
					Expect(got.Health).NotTo(Equal("unhealthy"))
				} else {
					Expect(got.Health).To(Equal("unhealthy"))
					Expect(got.HealthReason).To(ContainSubstring("the gateway got no tools"))
				}
			}
		})
	})

	It("gives each org with servers an executor key that opens its servers only, kept in a Secret koinon-api reads", func(ctx SpecContext) {
		server(ctx, org, "wiki", "", map[string]string{"read": "auto", "write": "ask", "drop": "never"})
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		st := mg.last()
		exec := gateway.AgentRef{Org: slug, Name: gateway.MCPExecAgentName}
		Expect(st.Tools[exec]).To(Equal([]gateway.ToolGrant{{Target: slug + "-wiki", Tools: []string{"read", "write"}}}),
			"the tools a person can approve a call of: ask tier, and auto tier for the leak guard's cards (ADR 0091); never the never tier")
		sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpservers.ExecSecretName(org), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec.Data[mcpservers.ExecKeyDataKey]).NotTo(BeEmpty())
		By("the same key is kept on the next pass")
		var before string
		for _, k := range st.Keys {
			if k.Agent == exec {
				before = k.ID
			}
		}
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		var after string
		for _, k := range mg.last().Keys {
			if k.Agent == exec {
				after = k.ID
			}
		}
		Expect(after).To(Equal(before))
		Expect(before).NotTo(BeEmpty())
	})

	It("offers no tool whose name agents could not use", func(ctx SpecContext) {
		s := server(ctx, org, "wiki", "", map[string]string{"fine": "auto", "has.dot": "auto", "a_very_long_tool_name_that_is_far_too_long_for_a_model_api_to_accept": "auto"})
		a := newAgent(ctx, org, user)
		allow(ctx, a, s, nil)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(mg.last().Tools[ref(slug, a)]).To(Equal([]gateway.ToolGrant{{Target: slug + "-wiki", Tools: []string{"fine"}}}))
	})

	It("does not serve a server whose URL the policy denies, and still serves the others", func(ctx SpecContext) {
		server(ctx, org, "good", "", map[string]string{"read": "auto"})
		bad := server(ctx, org, "bad", "", map[string]string{"read": "auto"})
		_, err := pool.Exec(ctx, `UPDATE mcp_servers SET url = 'http://127.0.0.1:9/mcp' WHERE id = $1`, bad.ID)
		Expect(err).NotTo(HaveOccurred())

		err = r.ReconcileAll(ctx)
		Expect(err).To(MatchError(ContainSubstring(slug + "/bad not served")))
		_, ok := targetOf(mg.last(), slug+"-good")
		Expect(ok).To(BeTrue())
		_, ok = targetOf(mg.last(), slug+"-bad")
		Expect(ok).To(BeFalse())
	})

	It("reserves the approval target names too: one org's server cannot take another's -ask name", func(ctx SpecContext) {
		// Org "x8a-b" with the server "ask" would be the target x8a-b-ask, the approval target of org
		// "x8a" with the server "b". The name "ask" is refused by the service, so force the row.
		prefix := uniq("x8a")
		mk := func(slug string) pgtype.UUID {
			o, _ := newOrgWithSlug(ctx, slug)
			return o
		}
		o1, o2 := mk(prefix+"-b"), mk(prefix)
		DeferCleanup(func(ctx SpecContext) {
			_, _ = pool.Exec(ctx, `DELETE FROM mcp_servers WHERE org_id IN ($1, $2)`, o1, o2)
		})
		_, err := svc.Create(ctx, o2, audit.System("koinon-test:spec"), pgtype.UUID{}, mcpservers.Input{Name: "b", URL: "https://mcp.example.com/x"})
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.Create(ctx, o1, audit.System("koinon-test:spec"), pgtype.UUID{}, mcpservers.Input{Name: "ask2", URL: "https://mcp.example.com/y"})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE mcp_servers SET name = 'ask' WHERE org_id = $1`, o1)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring(prefix + "-b-ask")))
		n := 0
		for _, t := range mg.last().MCP {
			if t.Name == prefix+"-b-ask" || t.Name == prefix+"-b" {
				n++
			}
		}
		Expect(n).To(Equal(1), "only the first org's server is served")
	})

	It("reports a gateway target name that two orgs would share, and serves the first only", func(ctx SpecContext) {
		// Org "x7a-b" with server "c" and org "x7a" with server "b-c" would both be the target x7a-b-c.
		mk := func(slug string) pgtype.UUID {
			o, _ := newOrgWithSlug(ctx, slug)
			return o
		}
		prefix := uniq("x7a")
		o1, o2 := mk(prefix+"-b"), mk(prefix)
		DeferCleanup(func(ctx SpecContext) {
			_, _ = pool.Exec(ctx, `DELETE FROM mcp_servers WHERE org_id IN ($1, $2)`, o1, o2)
		})
		for _, c := range []struct {
			org  pgtype.UUID
			name string
		}{{o1, "c"}, {o2, "b-c"}} {
			_, err := svc.Create(ctx, c.org, audit.System("koinon-test:spec"), pgtype.UUID{}, mcpservers.Input{Name: c.name, URL: "https://mcp.example.com/x"})
			Expect(err).NotTo(HaveOccurred())
		}
		err := r.ReconcileAll(ctx)
		Expect(err).To(MatchError(ContainSubstring(prefix + "-b-c")))
		n := 0
		for _, t := range mg.last().MCP {
			if t.Name == prefix+"-b-c" {
				n++
			}
		}
		Expect(n).To(Equal(1))
	})
})
