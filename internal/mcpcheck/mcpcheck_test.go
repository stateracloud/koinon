// SPDX-License-Identifier: Apache-2.0

package mcpcheck_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpcheck"
)

const (
	agentKey   = "koinon_test_not-a-real-token-agent"
	hmacSecret = "koinon_test_not-a-real-hmac-key-0123456789"
)

type keys map[string]string

func (k keys) AgentKey(_ context.Context, _, agent pgtype.UUID) (string, error) {
	v, ok := k[agent.String()]
	if !ok {
		return "", errors.New("no such key")
	}
	return v, nil
}

// fakeGateway is a stand-in for agentgateway: an MCP server that lists, for each key, only the tools
// the key is granted (a key without the grant sees none of a target's tools, as with failOpen).
func fakeGateway(grants map[string][]string) *httptest.Server {
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: "fake-gateway", Version: "0"}, nil)
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, name := range grants[key] {
			srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{}, nil
				})
		}
		return srv
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
}

var _ = Describe("The live check of an agent's MCP tools", func() {
	var org, agent pgtype.UUID
	BeforeEach(func() {
		Expect(org.Scan("11111111-1111-1111-1111-111111111111")).To(Succeed())
		Expect(agent.Scan("22222222-2222-2222-2222-222222222222")).To(Succeed())
	})

	serve := func(gw string, k keys, logs *bytes.Buffer) *httptest.Server {
		return httptest.NewServer(mcpcheck.Server{
			Keys: k, Gateway: gw + "/mcp",
			Key: func(context.Context) ([]byte, error) { return []byte(hmacSecret), nil },
			Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
	}
	remote := func(url string) mcpcheck.Remote {
		return mcpcheck.Remote{BaseURL: url, Key: func() ([]byte, error) { return []byte(hmacSecret), nil }}
	}

	It("lists the tools the gateway offers the agent's own key, and never shows the key", func(ctx SpecContext) {
		gw := fakeGateway(map[string][]string{agentKey: {"acme-tracker_create_issue"}, "koinon_test_other-key": {"acme-wiki_read"}})
		defer gw.Close()
		var logs bytes.Buffer
		api := serve(gw.URL, keys{agent.String(): agentKey}, &logs)
		defer api.Close()

		tools, err := remote(api.URL).Tools(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(tools).To(ConsistOf("acme-tracker_create_issue"))
		Expect(logs.String()).NotTo(ContainSubstring(agentKey))
	})

	It("lists nothing for a key that lacks the grant", func(ctx SpecContext) {
		gw := fakeGateway(map[string][]string{"koinon_test_other-key": {"acme-wiki_read"}})
		defer gw.Close()
		var logs bytes.Buffer
		api := serve(gw.URL, keys{agent.String(): agentKey}, &logs)
		defer api.Close()

		tools, err := remote(api.URL).Tools(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(tools).To(BeEmpty())
	})

	It("keeps the key out of the error and the log when the gateway is down", func(ctx SpecContext) {
		gw := fakeGateway(nil)
		gw.Close()
		var logs bytes.Buffer
		api := serve(gw.URL, keys{agent.String(): agentKey}, &logs)
		defer api.Close()

		_, err := remote(api.URL).Tools(ctx, org, agent)
		Expect(err).To(MatchError(ContainSubstring("502")))
		Expect(err.Error()).NotTo(ContainSubstring(agentKey))
		Expect(logs.String()).NotTo(ContainSubstring(agentKey))
	})

	It("answers 409 for an agent that has no key yet", func(ctx SpecContext) {
		gw := fakeGateway(nil)
		defer gw.Close()
		var logs bytes.Buffer
		api := serve(gw.URL, keys{}, &logs)
		defer api.Close()

		_, err := remote(api.URL).Tools(ctx, org, agent)
		Expect(err).To(MatchError(ContainSubstring("409")))
	})

	It("refuses a request signed with another key, an expired one, or one for another agent", func(ctx SpecContext) {
		gw := fakeGateway(map[string][]string{agentKey: {"acme-tracker_create_issue"}})
		defer gw.Close()
		var logs bytes.Buffer
		api := serve(gw.URL, keys{agent.String(): agentKey}, &logs)
		defer api.Close()

		wrong := mcpcheck.Remote{BaseURL: api.URL, Key: func() ([]byte, error) { return []byte("koinon_test_not-a-real-hmac-other-key-0000"), nil }}
		_, err := wrong.Tools(ctx, org, agent)
		Expect(err).To(MatchError(ContainSubstring("401")))

		stale := remote(api.URL)
		stale.Now = func() time.Time { return time.Now().Add(-time.Hour) }
		_, err = stale.Tools(ctx, org, agent)
		Expect(err).To(MatchError(ContainSubstring("401")))

		Expect(mcpcheck.ValidToken([]byte(hmacSecret), mcpcheck.Token([]byte(hmacSecret), org.String(), agent.String(), time.Now().Unix()+30),
			org.String(), "33333333-3333-3333-3333-333333333333", time.Now().Unix()+30, time.Now())).To(BeFalse())
	})
})
