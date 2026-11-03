// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type leakGuardJSON struct {
	MatchThreshold        int      `json:"matchThreshold"`
	DefaultMatchThreshold int      `json:"defaultMatchThreshold"`
	MinMatchThreshold     int      `json:"minMatchThreshold"`
	MaxMatchThreshold     int      `json:"maxMatchThreshold"`
	TrustedHosts          []string `json:"trustedHosts"`
}

var _ = Describe("the leak guard in the admin API (#350)", func() {
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
			Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("dataClass", ConsistOf("untrusted_input", "private_data", "outbound")))

			code, raw = e.call(ctx, e.token, http.MethodPost, "/mcp-servers", map[string]any{
				"name": "fetcher", "url": "https://mcp.example.com/mcp", "kind": "fetch", "dataClass": []string{"outbound", "untrusted_input"},
			})
			Expect(code).To(Equal(http.StatusCreated), string(raw))
			s := decode[map[string]any](raw)
			Expect(s).To(HaveKeyWithValue("kind", "fetch"))
			Expect(s).To(HaveKeyWithValue("dataClass", []any{"untrusted_input", "outbound"}))
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

	Describe("the settings and the fetch-trusted host list", func() {
		It("starts at 24 characters with an empty list", func(ctx SpecContext) {
			code, raw := e.call(ctx, e.token, http.MethodGet, "/leak-guard", nil)
			Expect(code).To(Equal(http.StatusOK), string(raw))
			l := decode[leakGuardJSON](raw)
			Expect(l.MatchThreshold).To(Equal(24))
			Expect(l.DefaultMatchThreshold).To(Equal(24))
			Expect(l.MinMatchThreshold).To(Equal(12))
			Expect(l.MaxMatchThreshold).To(Equal(256))
			Expect(string(raw)).To(ContainSubstring(`"trustedHosts":[]`))
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

			for _, bad := range []int{0, 11, 257, -5} {
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

		It("refuses a host that is not a bare DNS name", func(ctx SpecContext) {
			for _, bad := range []string{"https://docs.example.com/a", "*.example.com", "10.0.0.1", "docs.example.com:443", ""} {
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
