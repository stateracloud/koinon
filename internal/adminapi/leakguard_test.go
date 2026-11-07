// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

type leakGuardJSON struct {
	MatchThreshold        int      `json:"matchThreshold"`
	DefaultMatchThreshold int      `json:"defaultMatchThreshold"`
	MinMatchThreshold     int      `json:"minMatchThreshold"`
	MaxMatchThreshold     int      `json:"maxMatchThreshold"`
	TrustedHosts          []string `json:"trustedHosts"`
	CodeHostScope         struct {
		Repositories            int `json:"repositories"`
		Private                 int `json:"private"`
		Public                  int `json:"public"`
		Unknown                 int `json:"unknown"`
		VisibilityMaxAgeSeconds int `json:"visibilityMaxAgeSeconds"`
	} `json:"codeHostScope"`
}

var _ = Describe("the leak guard in the admin API (old issue 350)", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	auditCount := func(ctx SpecContext, action string) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2 AND actor_kind = 'token'`, e.org, action).Scan(&n)).To(Succeed())
		return n
	}

	Describe("the MCP server data class", func() {
		It("is unclassified at first, set on create and changed with an audited PUT", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{"name": "plain", "url": "https://mcp.example.com/mcp"})
			Expect(code).To(Equal(http.StatusCreated), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("kind", "other"))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("dataClass", ConsistOf("untrusted_input", "private_data")))

			code, raw = e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{
				"name": "fetcher", "url": "https://mcp.example.com/mcp", "kind": "fetch", "dataClass": []string{"untrusted_input"},
			})
			Expect(code).To(Equal(http.StatusCreated), string(raw))
			s := decode[map[string]any](raw)
			Expect(s).To(HaveKeyWithValue("kind", "fetch"))
			Expect(s).To(HaveKeyWithValue("dataClass", []any{"untrusted_input"}))
			id := s["id"].(string)

			code, raw = e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", map[string]any{"kind": "search", "dataClass": []string{}})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("kind", "search"))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("dataClass", BeEmpty()))
			code, raw = e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id, nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("kind", "search"))
			Expect(auditCount(ctx, "mcp_server.data_class.set")).To(Equal(1))
		})

		It("refuses an unknown kind or leg with 400 and an unknown server with 404", func(ctx SpecContext) {
			_, raw := e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{"name": "plain", "url": "https://mcp.example.com/mcp"})
			id := decode[mcpServerJSON](raw).ID
			for _, body := range []map[string]any{
				{"kind": "browser", "dataClass": []string{}},
				{"kind": "other", "dataClass": []string{"secrets"}},
				{"dataClass": []string{}},
			} {
				code, raw := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", body)
				Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			}
			code, _ := e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{"name": "bad", "url": "https://mcp.example.com/mcp", "dataClass": []string{"secrets"}})
			Expect(code).To(Equal(http.StatusBadRequest))
			Expect(auditCount(ctx, "mcp_server.data_class.set")).To(BeZero())

			code, _ = e.call(ctx, e.token, http.MethodPut, "/mcp-servers/00000000-0000-0000-0000-000000000000/data-class", map[string]any{"kind": "other", "dataClass": []string{}})
			Expect(code).To(Equal(http.StatusNotFound))
		})

		It("is org-scoped", func(ctx SpecContext) {
			_, raw := e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{"name": "plain", "url": "https://mcp.example.com/mcp"})
			id := decode[mcpServerJSON](raw).ID
			other := newEnv(ctx)
			code, _ := other.call(ctx, other.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", map[string]any{"kind": "fetch", "dataClass": []string{}})
			Expect(code).To(Equal(http.StatusNotFound))
			code, raw = e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id, nil)
			Expect(code).To(Equal(http.StatusOK))
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("kind", "other"))
		})
	})

	Describe("the sink class of a tool (ADR 0091)", func() {
		setup := func(ctx SpecContext) (id string) {
			GinkgoHelper()
			_, raw := e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{"name": "forge", "url": "https://mcp.example.com/mcp"})
			id = decode[mcpServerJSON](raw).ID
			_, err := e.mcp.SyncTools(ctx, e.org, pgOf(id), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object"}`)},
			})
			Expect(err).NotTo(HaveOccurred())
			return id
		}

		It("is undeclared at first: external-high, fixed, no address arguments", func(ctx SpecContext) {
			id := setup(ctx)
			code, raw := e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(string(raw)).To(ContainSubstring(`"sink":{"addressArgs":[],"derivation":"fixed","sinkClass":"external-high"}`))
		})

		// sinkBody is the request body for a declaration against the content hash the caller read.
		sinkBody := func(ctx SpecContext, id, tool string, sink map[string]any) map[string]any {
			GinkgoHelper()
			_, raw := e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			var list struct {
				Tools []struct {
					Name        string `json:"name"`
					ContentHash string `json:"contentHash"`
				} `json:"tools"`
			}
			Expect(json.Unmarshal(raw, &list)).To(Succeed(), string(raw))
			hash := "no-such-tool"
			for _, t := range list.Tools {
				if t.Name == tool {
					hash = t.ContentHash
				}
			}
			return map[string]any{"contentHash": hash, "sink": sink}
		}

		It("is declared with an audited PUT and read back", func(ctx SpecContext) {
			id := setup(ctx)
			code, raw := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/tools/create_issue/sink", sinkBody(ctx, id, "create_issue", map[string]any{
				"sinkClass": "external-high", "derivation": "code_host", "addressArgs": []string{"owner", "repo"}, "resultUntrustedInput": true,
			}))
			Expect(code).To(Equal(http.StatusOK), string(raw))
			sink := decode[struct{ Sink map[string]any }](raw).Sink
			Expect(sink).To(HaveKeyWithValue("derivation", "code_host"))
			Expect(sink).To(HaveKeyWithValue("addressArgs", []any{"owner", "repo"}))
			Expect(sink).To(HaveKeyWithValue("resultUntrustedInput", true))
			Expect(sink).NotTo(HaveKey("resultPrivateData"))

			code, raw = e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(string(raw)).To(ContainSubstring(`"derivation":"code_host"`))
			Expect(auditCount(ctx, "mcp_tool.declaration.set")).To(Equal(1))

			By("a changed server URL puts it back, audited")
			code, raw = e.call(ctx, e.token, http.MethodPatch, "/mcp-servers/"+id, map[string]any{"url": "https://other.example.com/mcp"})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			_, raw = e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			Expect(string(raw)).To(ContainSubstring(`"sink":{"addressArgs":[],"derivation":"fixed","sinkClass":"external-high"}`))
			Expect(auditCount(ctx, "mcp_tool.declaration.reset")).To(Equal(1))
		})

		It("answers 409 and changes nothing when the tool changed since the caller read it", func(ctx SpecContext) {
			id := setup(ctx)
			body := sinkBody(ctx, id, "create_issue", map[string]any{"sinkClass": "none", "derivation": "fixed", "addressArgs": []string{}})
			_, err := e.mcp.SyncTools(ctx, e.org, pgOf(id), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "Create an issue and mail the world", InputSchema: json.RawMessage(`{"type":"object"}`)},
			})
			Expect(err).NotTo(HaveOccurred())
			code, raw := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/tools/create_issue/sink", body)
			Expect(code).To(Equal(http.StatusConflict), string(raw))
			Expect(auditCount(ctx, "mcp_tool.declaration.set")).To(BeZero())
			_, raw = e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			Expect(string(raw)).To(ContainSubstring(`"sinkClass":"external-high"`))

			By("the hash of the new text goes through")
			code, raw = e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/tools/create_issue/sink", sinkBody(ctx, id, "create_issue", map[string]any{"sinkClass": "none", "derivation": "fixed", "addressArgs": []string{}}))
			Expect(code).To(Equal(http.StatusOK), string(raw))
		})

		It("refuses an invalid declaration with 400, an unknown tool with 404 and another org's server with 404", func(ctx SpecContext) {
			id := setup(ctx)
			for _, sink := range []map[string]any{
				{"sinkClass": "everywhere", "derivation": "fixed", "addressArgs": []string{}},
				{"sinkClass": "none", "derivation": "oracle", "addressArgs": []string{}},
				{"sinkClass": "external-high", "derivation": "code_host", "addressArgs": []string{"repo"}},
				{"sinkClass": "none", "derivation": "fixed", "addressArgs": []string{"a b"}},
			} {
				code, raw := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/tools/create_issue/sink", sinkBody(ctx, id, "create_issue", sink))
				Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			}
			Expect(auditCount(ctx, "mcp_tool.declaration.set")).To(BeZero())
			none := map[string]any{"sinkClass": "none", "derivation": "fixed", "addressArgs": []string{}}
			code, _ := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/tools/no_such/sink", sinkBody(ctx, id, "no_such", none))
			Expect(code).To(Equal(http.StatusNotFound))
			other := newEnv(ctx)
			code, _ = other.call(ctx, other.token, http.MethodPut, "/mcp-servers/"+id+"/tools/create_issue/sink", sinkBody(ctx, id, "create_issue", none))
			Expect(code).To(Equal(http.StatusNotFound))
			_, raw := e.call(ctx, e.token, http.MethodGet, "/mcp-servers/"+id+"/tools", nil)
			Expect(string(raw)).To(ContainSubstring(`"sinkClass":"external-high"`))
		})

		It("links a server to a code-host connection of the org, refuses another's, and clears the link with a new URL", func(ctx SpecContext) {
			id := setup(ctx)
			conn := uuid.New()
			_, err := store.New(pool).CreateConnection(ctx, store.CreateConnectionParams{
				// A GitHub Enterprise Server at the server's host, so the server may act through it (#117).
				OrgID: e.org, ID: pgtype.UUID{Bytes: conn, Valid: true}, Connector: "github", DisplayName: "gh", Config: []byte(`{"baseUrl":"https://mcp.example.com"}`),
				SecretName: pgtype.Text{String: "conn-" + conn.String(), Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())
			gitea := uuid.New()
			_, err = store.New(pool).CreateConnection(ctx, store.CreateConnectionParams{
				OrgID: e.org, ID: pgtype.UUID{Bytes: gitea, Valid: true}, Connector: "gitea", DisplayName: "forge", Config: []byte(`{"baseUrl":"https://git.example.com"}`),
				SecretName: pgtype.Text{String: "conn-" + gitea.String(), Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())

			code, raw := e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", map[string]any{"kind": "other", "dataClass": []string{}, "codeHostConnectionId": gitea.String()})
			Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			Expect(string(raw)).To(ContainSubstring("does not act on the Gitea connection"), "a server at another host than the connection's (#117)")

			code, raw = e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", map[string]any{"kind": "other", "dataClass": []string{"untrusted_input", "private_data"}, "codeHostConnectionId": conn.String()})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[struct {
				CodeHostConnectionID string `json:"codeHostConnectionId"`
			}](raw).CodeHostConnectionID).To(Equal(conn.String()))

			code, raw = e.call(ctx, e.token, http.MethodPut, "/mcp-servers/"+id+"/data-class", map[string]any{"kind": "other", "dataClass": []string{}, "codeHostConnectionId": uuid.NewString()})
			Expect(code).To(Equal(http.StatusBadRequest), string(raw))

			code, raw = e.call(ctx, e.token, http.MethodPatch, "/mcp-servers/"+id, map[string]any{"url": "https://other.example.com/mcp"})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(string(raw)).NotTo(ContainSubstring("codeHostConnectionId"))
		})
	})

	Describe("the settings and the fetch-trusted host list", func() {
		It("starts at 24 characters with an empty list and an empty code-host scope", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodGet, "/leak-guard", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			l := decode[leakGuardJSON](raw)
			Expect(l.MatchThreshold).To(Equal(24))
			Expect(l.DefaultMatchThreshold).To(Equal(24))
			Expect(l.MinMatchThreshold).To(Equal(12))
			Expect(l.MaxMatchThreshold).To(Equal(256))
			Expect(string(raw)).To(ContainSubstring(`"trustedHosts":[]`))
			Expect(l.CodeHostScope.Repositories).To(BeZero())
			Expect(l.CodeHostScope.VisibilityMaxAgeSeconds).To(Equal(3600))
		})

		It("sets the threshold and returns it to the default, auditing each change", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{"matchThreshold": 40})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).MatchThreshold).To(Equal(40))
			code, raw = e.call(ctx, e.token, http.MethodGet, "/leak-guard", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).MatchThreshold).To(Equal(40))

			code, raw = e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).MatchThreshold).To(Equal(24))
			Expect(auditCount(ctx, "org.leak_guard.threshold.set")).To(Equal(2))

			code, raw = e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{"matchThreshold": 40})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			code, raw = e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{"matchThreshold": 0})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).MatchThreshold).To(Equal(24), "0 returns the default, as on the UI API")
			for _, bad := range []int{11, 257, -5} {
				code, raw = e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{"matchThreshold": bad})
				Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			}
		})

		It("adds and removes trusted hosts, audited, and answers the new list each time", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodPost, "/leak-guard/trusted-hosts", map[string]any{"host": "Docs.Example.com"})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(Equal([]string{"docs.example.com"}))
			code, raw = e.call(ctx, e.token, http.MethodPost, "/leak-guard/trusted-hosts", map[string]any{"host": "api.example.org"})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(Equal([]string{"api.example.org", "docs.example.com"}))
			Expect(auditCount(ctx, "org.fetch_trusted_host.add")).To(Equal(2))

			code, raw = e.call(ctx, e.token, http.MethodDelete, "/leak-guard/trusted-hosts/docs.example.com", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(Equal([]string{"api.example.org"}))
			Expect(auditCount(ctx, "org.fetch_trusted_host.remove")).To(Equal(1))

			code, _ = e.call(ctx, e.token, http.MethodDelete, "/leak-guard/trusted-hosts/docs.example.com", nil)
			Expect(code).To(Equal(http.StatusNotFound))
		})

		It("adds and removes a wildcard entry", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodPost, "/leak-guard/trusted-hosts", map[string]any{"host": "*.Example.com"})
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(Equal([]string{"*.example.com"}))
			code, raw = e.call(ctx, e.token, http.MethodDelete, "/leak-guard/trusted-hosts/*.example.com", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(BeEmpty())
		})

		It("refuses a host that is not a bare DNS name", func(ctx SpecContext) {
			for _, bad := range []string{"https://docs.example.com/a", "*", "a.*.example.com", "*.com", "*.co.uk", "localhost", "127.1", "10.0.0.1", "docs.example.com:443", ""} {
				code, raw := e.call(ctx, e.token, http.MethodPost, "/leak-guard/trusted-hosts", map[string]any{"host": bad})
				Expect(code).To(Equal(http.StatusBadRequest), bad+": "+string(raw))
			}
			_, raw := e.call(ctx, e.token, http.MethodGet, "/leak-guard", nil)
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(BeEmpty())
		})

		It("is org-scoped: another org sees neither the host nor the threshold", func(ctx SpecContext) {
			code, _ := e.call(ctx, e.token, http.MethodPost, "/leak-guard/trusted-hosts", map[string]any{"host": "docs.example.com"})
			Expect(code).To(Equal(http.StatusOK))
			code, _ = e.call(ctx, e.token, http.MethodPut, "/leak-guard", map[string]any{"matchThreshold": 60})
			Expect(code).To(Equal(http.StatusOK))

			other := newEnv(ctx)
			code, raw := other.call(ctx, other.token, http.MethodGet, "/leak-guard", nil)
			Expect(code).To(Equal(http.StatusOK))
			l := decode[leakGuardJSON](raw)
			Expect(l.TrustedHosts).To(BeEmpty())
			Expect(l.MatchThreshold).To(Equal(24))
			code, _ = other.call(ctx, other.token, http.MethodDelete, "/leak-guard/trusted-hosts/docs.example.com", nil)
			Expect(code).To(Equal(http.StatusNotFound))

			_, raw = e.call(ctx, e.token, http.MethodGet, "/leak-guard", nil)
			Expect(decode[leakGuardJSON](raw).TrustedHosts).To(Equal([]string{"docs.example.com"}))
		})

		It("needs a bearer token", func(ctx SpecContext) {
			code, _ := e.call(ctx, "", http.MethodGet, "/leak-guard", nil)
			Expect(code).To(Equal(http.StatusUnauthorized))
		})
	})
})
