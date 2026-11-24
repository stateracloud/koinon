// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// leakCanary is private text of the fake kind, the same text as the guard-level control
// (internal/mcpguard, #1133): obviously not a secret, longer than the text-match threshold.
const leakCanary = "koinon_test_not-a-real-token is the vault passphrase koinon-canary-1051-amber-falcon-not-a-secret for the finance close"

// The canonical form the text match compares keeps letters and digits only. The default threshold t
// is 24 canonical characters (ADR 0091, amendment of 2026-11-16) and the k-gram is 21. Winnowing
// (internal/leakguard/doc.go) always catches a match of t or more characters, never one below k, and
// may catch one of k to t-1 (21 to 23), depending on the org's fingerprint key. The controls avoid
// that band: a test there would depend on the key.
var (
	canaryBelow = leakCanary[:25] // 20 canonical characters: below the k-gram, never caught
	canaryAt    = leakCanary[:30] // 24 canonical characters: the threshold
	canaryAbove = leakCanary[:31] // 25 canonical characters: one above the threshold
)

// canaryRemote is an MCP server with two tools: read_item answers with text, and search_items
// records its query and answers with no hits. It stands for the upstream: what search_items
// records is what left through the gateway.
type canaryRemote struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newCanaryRemote(name, text string) *canaryRemote {
	r := &canaryRemote{}
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "read_item", Description: "reads an item", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "search_items", Description: "searches the web"},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Query string `json:"query"`
		},
		) (*mcp.CallToolResult, any, error) {
			r.mu.Lock()
			r.queries = append(r.queries, in.Query)
			r.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no hits"}}}, nil, nil
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	r.Server = newServer(inner)
	return r
}

// Queries is every query search_items received, in order.
func (r *canaryRemote) Queries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

// leakCanarySpec is the leak guard's true-positive control on an external-low call through the
// gateway (#1051, ADR 0091): the real gateway, the real guard (internal/mcpguard.Server on Postgres)
// and fake upstream servers. The calls come from the spec, not from a model, so the control cannot
// pass because a model refused to send the canary. It runs in the Go suite of every pull request; the
// kind e2e (test/e2e, leakGuardCanaryInTask) runs the same control through a task, a harness and a
// scripted model.
//
// The session reads an untrusted page (untrusted-input leg) and a note that holds the canary
// (private-data leg), then searches on an external-low tool:
//
//   - positive control: a clean query, and the canary's first 20 canonical characters (below the
//     k-gram, so the text match cannot see it), pass and reach the upstream server;
//   - negative control: 24 (the threshold) and 25 canonical characters and the whole canary are each
//     held for an approval whose risk names the taint and the external-low class, and the upstream
//     server never sees them.
//
// The byte cap only alerts (#1058) and is not asserted here; internal/mcpguard covers it.
func leakCanarySpec(env Env) {
	Describe(env.Name+": the real MCP leak guard and a canary on an external-low call (#1051)", Ordered, func() {
		const (
			notes = "notes" // private data: its item is the canary
			web   = "web"   // untrusted input; search_items is external-low
		)
		var (
			url, key string
			logs     func(context.Context) string
			rg       *RealGuard
			webSrv   *canaryRemote
		)
		search := func(ctx context.Context, q string) (string, bool) {
			return callTool(ctx, url, key, rg.Target(web)+"_search_items", map[string]any{"query": q})
		}

		BeforeAll(func(ctx SpecContext) {
			rg = startRealGuard(ctx)
			notesSrv := newCanaryRemote(rg.Target(notes), "Finance close: "+leakCanary)
			DeferCleanup(func() { notesSrv.CloseClientConnections(); go notesSrv.Close() })
			webSrv = newCanaryRemote(rg.Target(web), "A public page about release notes.")
			DeferCleanup(func() { webSrv.CloseClientConnections(); go webSrv.Close() })
			rg.Server(ctx, notes, notesSrv.Server, []string{"private_data"}, "read_item")
			rg.Server(ctx, web, webSrv.Server, []string{"untrusted_input"}, "read_item", "search_items")
			rg.Declare(ctx, web, "search_items", sinkclass.Declaration{Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed})

			k, err := gateway.NewKey(rg.AgentRef())
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			host := func(s *httptest.Server) string { return "http://" + env.Host + ":" + itoa(port(s)) + "/mcp" }
			state := gateway.State{MCP: []gateway.MCPTarget{
				{Name: rg.Target(notes), Org: rg.Slug, URL: host(notesSrv.Server)},
				{Name: rg.Target(web), Org: rg.Slug, URL: host(webSrv.Server)},
			}}
			state.AddKey(k.Key)
			state.GrantTools(rg.AgentRef(),
				gateway.ToolGrant{Target: rg.Target(notes), Tools: []string{"read_item"}},
				gateway.ToolGrant{Target: rg.Target(web), Tools: []string{"read_item", "search_items"}})
			gw, err := env.Start(ctx, []int{port(notesSrv.Server), port(webSrv.Server), rg.Port()}, nil, rg.Ref(env.Host))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url, logs = gw.URL, gw.Logs
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(10*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		It("gives the session both legs: an untrusted page, then a note that holds the canary", func(ctx SpecContext) {
			text, failed := callTool(ctx, url, key, rg.Target(web)+"_read_item", nil)
			Expect(failed).To(BeFalse(), text)
			text, failed = callTool(ctx, url, key, rg.Target(notes)+"_read_item", nil)
			Expect(failed).To(BeFalse(), text)
			Expect(text).To(ContainSubstring(leakCanary), "the agent reads the note")
			var private, untrusted bool
			Expect(rg.Pool.QueryRow(ctx, `SELECT bool_or(private_data), bool_or(untrusted_input) FROM leak_guard_sessions
				WHERE org_id = $1 AND agent_id = $2`, rg.Org, rg.Agent.ID).Scan(&private, &untrusted)).To(Succeed())
			Expect(private).To(BeTrue())
			Expect(untrusted).To(BeTrue())
		})

		It("positive control: passes a clean query and a fragment below the k-gram to the upstream server", func(ctx SpecContext) {
			for _, q := range []string{"release notes 2026", canaryBelow} {
				text, failed := search(ctx, q)
				Expect(failed).To(BeFalse(), "%q: %s", q, text)
				Expect(text).To(Equal("no hits"))
			}
			Expect(webSrv.Queries()).To(Equal([]string{"release notes 2026", canaryBelow}))
			var approvals int
			Expect(rg.Pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE org_id = $1`, rg.Org).Scan(&approvals)).To(Succeed())
			Expect(approvals).To(BeZero())
		})

		It("negative control: holds the canary at, one above and far above the threshold for a taint approval of an external-low call", func(ctx SpecContext) {
			for _, q := range []string{canaryAt, canaryAbove, leakCanary} {
				text, failed := search(ctx, q)
				Expect(failed).To(BeTrue(), "the canary left through an external-low call: %q", q)
				Expect(text).To(ContainSubstring("approval requested: id="), "%q", q)
			}
			rows, err := rg.Pool.Query(ctx, `SELECT tool, risk FROM approval_requests WHERE org_id = $1 ORDER BY created_at, id`, rg.Org)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()
			n := 0
			for rows.Next() {
				var tool string
				var risk []byte
				Expect(rows.Scan(&tool, &risk)).To(Succeed())
				Expect(tool).To(Equal("search_items"))
				Expect(string(risk)).To(ContainSubstring(`"taint"`))
				Expect(string(risk)).To(MatchRegexp(`"sink_class":\s*"external-low"`))
				n++
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(n).To(Equal(3), "one approval for each held call")
		})

		It("never sends the canary to the upstream server", func() {
			Expect(webSrv.Queries()).To(Equal([]string{"release notes 2026", canaryBelow}), "only the two clean calls left")
			for _, q := range webSrv.Queries() {
				Expect(q).NotTo(ContainSubstring(canaryAt))
				Expect(q).NotTo(ContainSubstring("amber-falcon"))
			}
		})
	})
}
