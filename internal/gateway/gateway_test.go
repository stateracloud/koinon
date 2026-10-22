// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/gateway"
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
})
