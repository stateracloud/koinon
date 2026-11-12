// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// dollarSpec states what a driver does with a '$' in an org-supplied string it would write into the
// gateway's configuration (#668): a provider URL, an MCP server URL, or an MCP credential header
// name or prefix. agentgateway's file loader reads $NAME and ${NAME} as the gateway's environment
// variables, so such a value could pull the gateway's environment into an upstream request, or fail
// the load of the config every org shares. The org-facing validators refuse '$' when the value is
// saved; a state that carries one anyway is refused by Apply, naming the field, and the gateway
// keeps serving the revision it had.
func dollarSpec(env Env) {
	Describe(env.Name+": an org string with '$' is refused, and the served revision stays (#668)", Ordered, func() {
		var (
			gw    *Gateway
			state gateway.State
			key   string
			hits  atomic.Int64
		)
		BeforeAll(func(ctx SpecContext) {
			upstream := fakeUpstream(&hits)
			DeferCleanup(upstream.Close)
			state = gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "dollar", Type: "fake", BaseURL: fmt.Sprintf("http://%s:%d/v1", env.Host, port(upstream)),
				Models: []gateway.Model{{Name: testModel, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			}}}
			k, err := gateway.NewKey(agentA)
			Expect(err).NotTo(HaveOccurred())
			state.AddKey(k.Key)
			key = k.Material
			gw, err = env.Start(ctx, []int{port(upstream)}, nil, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		status := func(ctx context.Context) int {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, testModel)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewBufferString(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp.StatusCode
		}

		// with returns a copy of the served state with one field changed by set.
		with := func(set func(*gateway.State)) gateway.State {
			s := state
			s.Providers = append([]gateway.Provider(nil), state.Providers...)
			set(&s)
			return s
		}
		target := func(url, header, prefix string) func(*gateway.State) {
			return func(s *gateway.State) {
				t := gateway.MCPTarget{Name: "acme-wiki", Org: "acme", URL: url, CredentialHeader: header, CredentialPrefix: prefix}
				if header != "" {
					t.CredentialFile = env.KeyFile("mcp_wiki")
				}
				s.MCP = []gateway.MCPTarget{t}
			}
		}
		const remote = "https://mcp.example.com/mcp"
		cases := []struct {
			field string
			set   func(*gateway.State)
		}{
			{"base URL", func(s *gateway.State) { s.Providers[0].BaseURL += "/${HOME}" }},
			{"URL", target("https://mcp.example.com/$HOME/mcp", "", "")},
			{"credential header", target(remote, "X-Api-$HOME", "")},
			{"credential prefix", target(remote, "Authorization", "Bearer ${HOME} ")},
		}

		It("refuses each field with '$', naming it, and keeps serving the previous revision", func(ctx SpecContext) {
			Expect(status(ctx)).To(Equal(http.StatusOK), "before")
			for _, c := range cases {
				_, err := gw.Driver.Apply(ctx, with(c.set))
				Expect(err).To(MatchError(ContainSubstring(c.field+" contains '$'")), c.field)
				Expect(status(ctx)).To(Equal(http.StatusOK), "after the refused Apply with '$' in the %s", c.field)
			}
		}, SpecTimeout(3*time.Minute))
	})
}
