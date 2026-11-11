// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
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

// The tool servers trust gateway.AgentHeader, so what they parse from it must be exactly one agent. A
// client copy that the gateway failed to overwrite shows up as a joined value; it must not resolve (#281).
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
		Entry("two agents joined by a second copy of the header", agent.String()+", other/victim"),
		Entry("a path segment", "acme/repo-helper/extra"),
		Entry("a newline", "acme/repo-helper\nx-koinon-org: evil"),
	)
})
