// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/gateway/standalone"
)

var _ = Describe("Render", func() {
	for _, f := range fixtures() {
		It("matches testdata/"+f.name+".yaml", func() {
			got, rev, err := standalone.Render(f.state, f.options)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(ContainSubstring(string(rev)))

			path := filepath.Join("testdata", f.name+".yaml")
			if *update {
				Expect(os.WriteFile(path, got, 0o600)).To(Succeed())
			}
			want, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred(), "run with -args -update to create it")
			Expect(string(got)).To(Equal(string(want)))
		})
	}

	It("is deterministic and independent of key order", func() {
		f := fixtures()[2]
		a, revA, err := standalone.Render(f.state, f.options)
		Expect(err).NotTo(HaveOccurred())

		s := f.state
		s.Keys = []gateway.Key{s.Keys[1], s.Keys[0]}
		b, revB, err := standalone.Render(s, f.options)
		Expect(err).NotTo(HaveOccurred())
		Expect(b).To(Equal(a))
		Expect(revB).To(Equal(revA))
	})

	It("changes the revision when a key is revoked", func() {
		f := fixtures()[2]
		_, before, err := standalone.Render(f.state, f.options)
		Expect(err).NotTo(HaveOccurred())
		s := f.state
		s.Keys = append([]gateway.Key(nil), s.Keys...)
		Expect(s.RevokeKey("k-bbbbbbbbbbbb")).To(BeTrue())
		out, after, err := standalone.Render(s, f.options)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
		Expect(string(out)).NotTo(ContainSubstring("k-bbbbbbbbbbbb"))
	})

	It("never contains key material", func() {
		k, err := gateway.NewKey(agentA)
		Expect(err).NotTo(HaveOccurred())
		s := fixtures()[2].state
		s.AddKey(k.Key)
		out, _, err := standalone.Render(s, fixtures()[2].options)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring(k.Hash))
		Expect(string(out)).NotTo(ContainSubstring(k.Material))
	})

	It("requires a database for limits", func() {
		s := gateway.State{}
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 1, Window: time.Hour})
		_, _, err := standalone.Render(s, standalone.Options{})
		Expect(err).To(MatchError(ContainSubstring("DatabaseURL")))
	})

	It("rejects values that could escape CEL or YAML", func() {
		s := gateway.State{MCP: []gateway.MCPTarget{{Name: "github", URL: "http://x/mcp"}}}
		s.GrantTools(agentA, gateway.ToolGrant{Target: "github", Tools: []string{`x"] || true || ["`}})
		_, _, err := standalone.Render(s, standalone.Options{})
		Expect(err).To(MatchError(ContainSubstring("invalid tool name")))

		s = gateway.State{}
		s.AddKey(gateway.Key{ID: "k-1", Agent: gateway.AgentRef{Org: "acme", Name: `a" || true`}, Hash: gateway.HashKey("x")})
		_, _, err = standalone.Render(s, standalone.Options{})
		Expect(err).To(MatchError(ContainSubstring("invalid agent ref")))
	})
})
