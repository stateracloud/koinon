// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
)

func TestGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Suite")
}

var agent = gateway.AgentRef{Org: "acme", Name: "repo-helper"}

var _ = Describe("NewKey", func() {
	It("returns material once with its sha256 hash", func() {
		k, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(k.Material).To(HavePrefix(gateway.KeyPrefix))
		Expect(len(k.Material)).To(BeNumerically(">=", len(gateway.KeyPrefix)+43))
		sum := sha256.Sum256([]byte(k.Material))
		Expect(k.Hash).To(Equal("sha256:" + hex.EncodeToString(sum[:])))
		Expect(k.Agent).To(Equal(agent))
		Expect(k.ID).To(MatchRegexp(`^k-[0-9a-f]{12}$`))
	})

	It("generates distinct keys", func() {
		a, _ := gateway.NewKey(agent)
		b, _ := gateway.NewKey(agent)
		Expect(a.Material).NotTo(Equal(b.Material))
		Expect(a.ID).NotTo(Equal(b.ID))
	})

	It("refuses invalid agent refs", func() {
		_, err := gateway.NewKey(gateway.AgentRef{Org: "Acme", Name: "x"})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("State", func() {
	It("revokes keys and sets limits", func() {
		var s gateway.State
		a, _ := gateway.NewKey(agent)
		b, _ := gateway.NewKey(agent)
		s.AddKey(a.Key)
		s.AddKey(b.Key)
		Expect(s.RevokeKey(a.ID)).To(BeTrue())
		Expect(s.RevokeKey(a.ID)).To(BeFalse())
		Expect(s.Keys).To(Equal([]gateway.Key{b.Key}))

		s.SetLimit(agent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: time.Hour})
		Expect(s.Limits).To(HaveKey(agent))
		s.SetLimit(agent, nil)
		Expect(s.Limits).NotTo(HaveKey(agent))
		Expect(s.Validate()).To(Succeed())
	})

	It("rounds a USD limit down to the gateway's nine fraction digits", func() {
		l := gateway.USDLimit(money.MustParse("4.9827242408"), time.Hour, "g")
		Expect(l.Amount).To(Equal(money.MustParse("4.98272424")))
		Expect(l.Validate()).To(Succeed())
		l.Amount = money.MustParse("4.9827242408")
		Expect(l.Validate()).To(MatchError(ContainSubstring("more than 9 fraction digits")))
		Expect((gateway.Limit{Unit: gateway.USD, Amount: -1}).Validate()).To(HaveOccurred())
		Expect((gateway.Limit{Unit: "Tokens", Amount: 1}).Validate()).To(MatchError(ContainSubstring("unknown unit")))
	})

	It("takes the keys of agents with an invalid limit out, one rejection each", func() {
		var s gateway.State
		bad, _ := gateway.NewKey(agent)
		good, _ := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "other"})
		s.AddKey(bad.Key)
		s.AddKey(good.Key)
		s.SetLimit(agent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.0000000001"), Window: time.Hour})
		s.SetLimit(good.Agent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: time.Hour})
		out, rej := s.WithoutInvalidLimits()
		Expect(out.Keys).To(Equal([]gateway.Key{good.Key}))
		Expect(out.Limits).To(HaveKey(good.Agent))
		Expect(out.Limits).NotTo(HaveKey(agent))
		Expect(rej).To(HaveLen(1))
		Expect(rej[0].Kind).To(Equal("Key"))
		Expect(rej[0].Name).To(Equal(bad.ID))
		Expect(s.Limits).To(HaveKey(agent), "the input is not changed")
		Expect(s.Validate()).NotTo(Succeed())
		Expect(out.Validate()).To(Succeed())
		same, none := out.WithoutInvalidLimits()
		Expect(none).To(BeEmpty())
		Expect(same.Keys).To(Equal(out.Keys))
	})

	// ADR 0097 is silent on this; the maintainer decides (#279 slice 2). A key is shared by both halves
	// (State.Keys), so dropping an agent's key for a bad model limit also takes the agent off the MCP
	// path: fail closed, the same as before the split, when one key served both. The agent's tool
	// grants stay in the MCP half but name no key, so nothing can reach them.
	It("takes the agent off the MCP path too when its model limit is invalid (keys are shared by the halves)", func() {
		var s gateway.State
		bad, _ := gateway.NewKey(agent)
		good, _ := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "other"})
		s.AddKey(bad.Key)
		s.AddKey(good.Key)
		s.MCP = []gateway.MCPTarget{{Name: "wiki", URL: "http://wiki/mcp"}}
		s.GrantTools(agent, gateway.ToolGrant{Target: "wiki", All: true})
		s.GrantTools(good.Agent, gateway.ToolGrant{Target: "wiki", All: true})
		s.SetLimit(agent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.0000000001"), Window: time.Hour})

		out, rej := s.WithoutInvalidLimits()
		Expect(rej).To(HaveLen(1))
		Expect(out.MCPConfig().Keys).To(Equal([]gateway.Key{good.Key}), "the MCP half is not handed the dropped key")
		Expect(out.ModelConfig().Keys).To(Equal(out.MCPConfig().Keys), "both halves see the same keys")
		Expect(out.Tools).To(HaveKey(agent), "the grants are untouched; without a key nothing reaches them")
		Expect(out.Validate()).To(Succeed())
	})

	It("rejects grants for unknown targets and duplicate keys", func() {
		var s gateway.State
		k, _ := gateway.NewKey(agent)
		s.AddKey(k.Key)
		s.AddKey(k.Key)
		s.GrantTools(agent, gateway.ToolGrant{Target: "nope", Tools: []string{"x"}})
		err := s.Validate()
		Expect(err).To(HaveOccurred())
		Expect(strings.Count(err.Error(), "\n")).To(BeNumerically(">=", 1))
		Expect(err.Error()).To(And(ContainSubstring("duplicate"), ContainSubstring(`unknown mcp target "nope"`)))
	})

	It("refuses an unknown model format and a malformed workspace ID, and and an unset one", func() {
		provider := func(format modelformat.Format, workspace string) gateway.State {
			return gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "p", Type: "anthropic", BaseURL: "http://p/v1", AnthropicWorkspaceID: workspace,
				Models: []gateway.Model{{Name: "m", Format: format}},
			}}}
		}
		Expect(provider("", "").Validate()).To(MatchError(ContainSubstring("unset format")))
		Expect(provider(modelformat.OpenAI, "").Validate()).To(Succeed())
		Expect(provider(modelformat.Anthropic, "wrkspc_01AbC").Validate()).To(Succeed())
		Expect(provider("gemini", "").Validate()).To(MatchError(ContainSubstring(`format "gemini"`)))
		for _, bad := range []string{"wrkspc_", "ws_01", "wrkspc_01\r\nx-evil: 1", "wrkspc_01 "} {
			Expect(provider(modelformat.Anthropic, bad).Validate()).To(MatchError(ContainSubstring("workspace id")), bad)
		}
	})

	It("refuses a thinking capability the table could not hold and a beta that cannot be a header value", func() {
		model := func(c thinking.Capability) gateway.State {
			return gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "p", Type: "anthropic", BaseURL: "http://p/v1",
				Models: []gateway.Model{{Name: "m", Format: modelformat.Anthropic, Thinking: c}},
			}}}
		}
		Expect(model(thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic)).Validate()).To(Succeed())
		Expect(model(thinking.Capability{}).Validate()).To(Succeed(), "the zero value is a model that does not think")
		Expect(model(thinking.Capability{Thinks: thinking.Always, On: "extreme"}).Validate()).To(MatchError(ContainSubstring(`thinking mode "extreme"`)))
		Expect(model(thinking.Capability{Thinks: thinking.Always, On: thinking.AdaptiveSummarized, Off: thinking.Disabled}).Validate()).To(MatchError(ContainSubstring("no off form")))
		for _, bad := range []string{"", "a b", "x\r\nevil: 1", "UPPER", strings.Repeat("a", 200)} {
			Expect(model(thinking.Capability{Thinks: thinking.Always, On: thinking.AdaptiveSummarized, Betas: []string{bad}}).Validate()).To(MatchError(ContainSubstring("invalid anthropic beta")), bad)
		}
	})

	It("reserves the reload probe's key name", func() {
		var s gateway.State
		s.AddKey(gateway.Key{ID: gateway.ReloadProbeKeyName, Agent: agent, Hash: gateway.HashKey("x")})
		Expect(s.Validate()).To(MatchError(ContainSubstring("reserved")))
	})
})

var _ = Describe("AgentRef", func() {
	It("uses the orgs.slug CHECK of the initial migration for the org", func() {
		sql, err := os.ReadFile("../db/migrations/00001_initial.sql")
		Expect(err).NotTo(HaveOccurred())
		m := regexp.MustCompile(`slug\s+text[^\n]*\n\s*CHECK \(slug ~ '([^']+)'\)`).FindSubmatch(sql)
		Expect(m).NotTo(BeNil(), "orgs.slug CHECK not found")
		Expect(string(m[1])).To(Equal(gateway.OrgSlugPattern))
	})

	DescribeTable("validates org slugs like Postgres", func(org string, ok bool) {
		err := gateway.AgentRef{Org: org, Name: "repo-helper"}.Validate()
		if ok {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(HaveOccurred())
		}
	},
		Entry("short", "a", true),
		Entry("40 characters", strings.Repeat("a", 40), true),
		Entry("41 characters", strings.Repeat("a", 41), false),
		Entry("63 characters", strings.Repeat("a", 63), false),
		Entry("leading dash", "-acme", false),
		Entry("upper case", "Acme", false),
	)

	It("still allows 63-character agent names", func() {
		Expect(gateway.AgentRef{Org: "acme", Name: strings.Repeat("a", 63)}.Validate()).To(Succeed())
		Expect(gateway.AgentRef{Org: "acme", Name: strings.Repeat("a", 64)}.Validate()).NotTo(Succeed())
	})
})

// The tool servers trust gateway.AgentHeader, so what they read from it must be exactly one agent (#281).
var _ = Describe("ParseAgentRef on the agent header", func() {
	It("accepts what the gateway sets", func() {
		got, err := gateway.ParseAgentRef(agent.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(agent))
	})
	DescribeTable("refuses a value that is not one agent",
		func(v string) {
			_, err := gateway.ParseAgentRef(v)
			Expect(err).To(HaveOccurred())
		},
		Entry("empty (a request that skipped the gateway)", ""),
		Entry("a path segment", "acme/repo-helper/extra"),
		Entry("a newline", "acme/repo-helper\nx-koinon-org: evil"),
	)
})

// Go keeps repeated header lines apart and Get returns the first, so a client copy that a driver
// appended to (instead of overwriting) must make the whole header unusable.
var _ = Describe("AgentFromHeader", func() {
	It("accepts the one value the gateway sets", func() {
		got, err := gateway.AgentFromHeader(http.Header{gateway.AgentHeader: {agent.String()}})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(agent))
	})
	It("refuses a missing header and a nil one", func() {
		_, err := gateway.AgentFromHeader(http.Header{})
		Expect(err).To(HaveOccurred())
		_, err = gateway.AgentFromHeader(nil)
		Expect(err).To(HaveOccurred())
	})
	It("refuses two separate values, whichever comes first", func() {
		h := http.Header{}
		h.Add(gateway.AgentHeader, "evil/victim")
		h.Add(gateway.AgentHeader, agent.String())
		_, err := gateway.AgentFromHeader(h)
		Expect(err).To(HaveOccurred())
		_, err = gateway.AgentFromHeader(http.Header{gateway.AgentHeader: {agent.String(), "evil/victim"}})
		Expect(err).To(HaveOccurred())
	})
	It("refuses one value that is not an agent", func() {
		_, err := gateway.AgentFromHeader(http.Header{gateway.AgentHeader: {"acme/a/b"}})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("State halves (ADR 0097 decision 2.1)", func() {
	hash := "sha256:" + strings.Repeat("a", 64)
	key := gateway.Key{ID: "k1", Agent: agent, Hash: hash}
	target := gateway.MCPTarget{Name: "tools", URL: "http://tools.invalid/mcp"}
	provider := gateway.Provider{Org: "acme", Name: "p", Type: "openai", BaseURL: "http://p.invalid"}

	It("keeps each setter on its own half and leaves the other unchanged", func() {
		var s gateway.State
		s.MCP = []gateway.MCPTarget{target}
		s.Providers = []gateway.Provider{provider}
		mcpBefore := s.MCPState
		s.SetLimit(agent, &gateway.Limit{})
		s.RestrictModels(agent, "m")
		Expect(s.MCPState).To(Equal(mcpBefore))
		Expect(s.ModelState.Limits).To(HaveKey(agent))
		Expect(s.ModelState.Models).To(HaveKey(agent))

		modelBefore := s.ModelState
		s.GrantTools(agent, gateway.ToolGrant{Target: "tools", Tools: []string{"x"}})
		Expect(s.ModelState).To(Equal(modelBefore))
		Expect(s.MCPState.Tools).To(HaveKey(agent))
	})

	It("gives each path a view with the same keys and only its half", func() {
		s := gateway.State{Keys: []gateway.Key{key}}
		s.Providers = []gateway.Provider{provider}
		s.MCP = []gateway.MCPTarget{target}
		mc, tc := s.ModelConfig(), s.MCPConfig()
		Expect(mc.Keys).To(Equal(s.Keys))
		Expect(tc.Keys).To(Equal(s.Keys))
		Expect(mc.Providers).To(Equal(s.Providers))
		Expect(tc.MCP).To(Equal(s.MCP))
	})

	It("reports in each half's Validate only that half's faults", func() {
		s := gateway.State{Keys: []gateway.Key{key}}
		s.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "openai"}}
		s.MCP = []gateway.MCPTarget{{Name: "bad name", URL: ""}}
		Expect(s.Validate()).To(MatchError(And(
			ContainSubstring("base URL is required"), ContainSubstring("invalid name or empty URL"))))

		err := s.ModelConfig().Validate()
		Expect(err).To(MatchError(ContainSubstring("base URL is required")))
		Expect(err).NotTo(MatchError(ContainSubstring("invalid name or empty URL")))

		err = s.MCPConfig().Validate()
		Expect(err).To(MatchError(ContainSubstring("invalid name or empty URL")))
		Expect(err).NotTo(MatchError(ContainSubstring("base URL is required")))
	})

	It("lists State.Validate faults as keys, limits, providers, models, MCP targets, tools", func() {
		s := gateway.State{Keys: []gateway.Key{{ID: "k1", Agent: agent, Hash: "nope"}}}
		s.SetLimit(agent, &gateway.Limit{})
		s.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "openai"}}
		s.RestrictModels(agent, "bad name!")
		s.MCP = []gateway.MCPTarget{{Name: "bad name", URL: ""}}
		s.GrantTools(agent, gateway.ToolGrant{Target: "nowhere", Tools: []string{"x"}})
		err := s.Validate()
		Expect(err).To(HaveOccurred())
		msg := err.Error()
		var at []int
		for _, part := range []string{
			"hash must be", "limit for", "base URL is required", "models for",
			"invalid name or empty URL", "unknown mcp target",
		} {
			i := strings.Index(msg, part)
			Expect(i).To(BeNumerically(">=", 0), part)
			at = append(at, i)
		}
		Expect(slices.IsSorted(at)).To(BeTrue(), "fault order in %q", msg)
	})

	It("reports a key fault through both views", func() {
		s := gateway.State{Keys: []gateway.Key{{ID: "k1", Agent: agent, Hash: "nope"}}}
		Expect(s.ModelConfig().Validate()).To(MatchError(ContainSubstring("hash must be")))
		Expect(s.MCPConfig().Validate()).To(MatchError(ContainSubstring("hash must be")))
	})

	It("validates both halves through State.Validate", func() {
		s := gateway.State{
			Keys:     []gateway.Key{key},
			MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{{Name: "bad name", URL: ""}}},
		}
		s.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "openai"}}
		err := s.Validate()
		Expect(err).To(MatchError(ContainSubstring("base URL is required")))
		Expect(err).To(MatchError(ContainSubstring("invalid name or empty URL")))
	})
})

// agentgateway's standalone config loader expands $NAME and ${NAME} over the whole file, so the
// org-supplied strings a driver writes there refuse '$' at the one check every driver passes (#668).
var _ = Describe("State.Validate and '$'", func() {
	provider := func(url string) gateway.State {
		return gateway.State{Providers: []gateway.Provider{{Org: "acme", Name: "openrouter", Type: "openrouter", BaseURL: url}}}
	}
	target := func(url, header, prefix string) gateway.State {
		t := gateway.MCPTarget{Name: "acme-wiki", Org: "acme", URL: url, CredentialHeader: header, CredentialPrefix: prefix}
		if header != "" {
			t.CredentialFile = "/secrets/upstream/mcp_wiki"
		}
		return gateway.State{MCP: []gateway.MCPTarget{t}}
	}

	It("accepts the same fields without '$'", func() {
		Expect(provider("https://openrouter.ai/api/v1").Validate()).To(Succeed())
		Expect(target("https://mcp.example.com/mcp", "X-Api-Key", "Token ").Validate()).To(Succeed())
	})

	DescribeTable("refuses '$' in each field, naming it",
		func(s gateway.State, want string) {
			err := s.Validate()
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(MatchError(ContainSubstring("environment variable")))
		},
		Entry("the provider base URL", provider("https://openrouter.ai/api/${HOME}/v1"), `provider "acme/openrouter": base URL contains '$'`),
		Entry("the MCP URL", target("https://mcp.example.com/$HOME", "", ""), `mcp target "acme-wiki": URL contains '$'`),
		Entry("the MCP credential header", target("https://mcp.example.com/mcp", "X-$HOME", ""), `mcp target "acme-wiki": credential header contains '$'`),
		Entry("the MCP credential prefix", target("https://mcp.example.com/mcp", "Authorization", "Bearer $$ "), `mcp target "acme-wiki": credential prefix contains '$'`),
	)
})
