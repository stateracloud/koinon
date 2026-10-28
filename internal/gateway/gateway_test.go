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

		s.SetLimit(agent, &gateway.Limit{Unit: gateway.USD, Amount: 1, Window: time.Hour})
		Expect(s.Limits).To(HaveKey(agent))
		s.SetLimit(agent, nil)
		Expect(s.Limits).NotTo(HaveKey(agent))
		Expect(s.Validate()).To(Succeed())
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

	It("refuses an unknown model format and a malformed workspace ID, and takes an unset format as openai", func() {
		provider := func(format modelformat.Format, workspace string) gateway.State {
			return gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "p", Type: "anthropic", BaseURL: "http://p/v1", AnthropicWorkspaceID: workspace,
				Models: []gateway.Model{{Name: "m", Format: format}},
			}}}
		}
		Expect(provider("", "").Validate()).To(Succeed())
		Expect(provider(modelformat.OpenAI, "").Validate()).To(Succeed())
		Expect(provider(modelformat.Anthropic, "wrkspc_01AbC").Validate()).To(Succeed())
		Expect(provider("gemini", "").Validate()).To(MatchError(ContainSubstring(`unknown format "gemini"`)))
		for _, bad := range []string{"wrkspc_", "ws_01", "wrkspc_01\r\nx-evil: 1", "wrkspc_01 "} {
			Expect(provider(modelformat.Anthropic, bad).Validate()).To(MatchError(ContainSubstring("workspace id")), bad)
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
