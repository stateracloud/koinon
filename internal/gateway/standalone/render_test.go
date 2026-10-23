// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
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

	It("serves only priced models, so none is charged zero (L1)", func() {
		out, _, err := standalone.Render(fixtures()[2].state, fixtures()[2].options)
		Expect(err).NotTo(HaveOccurred())
		Expect(fixtures()[2].state.Models.Unpriced()).To(Equal([]string{"unpriced/model"}))
		Expect(string(out)).NotTo(ContainSubstring("unpriced/model"))

		onlyUnpriced := gateway.State{Models: gateway.ModelUpstream{
			Provider: "x", BaseURL: "http://x/v1", Models: []gateway.Model{{Name: "free"}},
		}}
		out, _, err = standalone.Render(onlyUnpriced, standalone.Options{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).NotTo(ContainSubstring("free"))
		Expect(string(out)).To(ContainSubstring("  models: []\n"))
	})

	Describe("the reload probe key", func() {
		type key struct {
			KeyHash  string `yaml:"keyHash"`
			Metadata struct {
				Name  string `yaml:"name"`
				Agent string `yaml:"agent"`
			} `yaml:"metadata"`
			Budgets []map[string]any `yaml:"budgets"`
		}
		type policies struct {
			APIKey struct {
				Keys []key `yaml:"keys"`
			} `yaml:"apiKey"`
			Authorization struct {
				Rules []string `yaml:"rules"`
			} `yaml:"authorization"`
		}
		type doc struct {
			Config struct {
				AdminAddr string `yaml:"adminAddr"`
			} `yaml:"config"`
			LLM *struct {
				Policies policies `yaml:"policies"`
			} `yaml:"llm"`
			MCP *struct {
				Policies policies `yaml:"policies"`
			} `yaml:"mcp"`
		}
		parse := func(s gateway.State, o standalone.Options) (doc, gateway.Revision) {
			GinkgoHelper()
			out, rev, err := standalone.Render(s, o)
			Expect(err).NotTo(HaveOccurred())
			var d doc
			Expect(yaml.Unmarshal(out, &d)).To(Succeed())
			return d, rev
		}
		probes := func(keys []key) []key {
			var out []key
			for _, k := range keys {
				if k.Metadata.Name == gateway.ReloadProbeKeyName {
					out = append(out, k)
				}
			}
			return out
		}

		for _, f := range fixtures() {
			It("is in "+f.name+" under llm only, for its revision", func() {
				d, rev := parse(f.state, f.options)
				Expect(d.Config.AdminAddr).To(Equal("localhost:15000"))
				Expect(d.LLM).NotTo(BeNil())
				p := probes(d.LLM.Policies.APIKey.Keys)
				Expect(p).To(HaveLen(1))
				Expect(p[0].KeyHash).To(Equal(gateway.HashKey(standalone.ReloadProbeKey(rev))))
				Expect(p[0].Metadata.Agent).To(BeEmpty())
				Expect(d.LLM.Policies.Authorization.Rules).To(Equal([]string{`apiKey.name != "koinon-reload-probe"`}))
				if d.MCP != nil {
					Expect(probes(d.MCP.Policies.APIKey.Keys)).To(BeEmpty())
				}
				if f.options.DatabaseURL == "" {
					// agentgateway refuses budgets without a database.
					Expect(p[0].Budgets).To(BeEmpty())
				} else {
					Expect(p[0].Budgets).To(Equal([]map[string]any{{
						"name":             "koinon-reload-probe",
						"limit":            map[string]any{"unit": "Tokens", "amount": 0},
						"window":           map[string]any{"rolling": "24h"},
						"onBudgetExceeded": "Block",
					}}))
				}
			})
		}

		It("changes with the revision", func() {
			f := fixtures()[2]
			a, revA := parse(f.state, f.options)
			s := f.state
			s.Keys = append([]gateway.Key(nil), s.Keys...)
			Expect(s.RevokeKey("k-bbbbbbbbbbbb")).To(BeTrue())
			b, revB := parse(s, f.options)
			Expect(revB).NotTo(Equal(revA))
			Expect(probes(b.LLM.Policies.APIKey.Keys)[0].KeyHash).NotTo(Equal(probes(a.LLM.Policies.APIKey.Keys)[0].KeyHash))
			Expect(standalone.ReloadProbeKey(revA)).NotTo(Equal(standalone.ReloadProbeKey(revB)))
		})
	})

	It("names the budget after the limit's generation", func() {
		s := gateway.State{Models: fixtures()[2].state.Models}
		s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 1, Window: 24 * time.Hour, Generation: "g1"})
		o := standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"}
		a, _, err := standalone.Render(s, o)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(a)).To(ContainSubstring("name: koinon-agent-limit-g1\n"))

		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 1, Window: 24 * time.Hour, Generation: "Bad_Gen"})
		_, _, err = standalone.Render(s, o)
		Expect(err).To(MatchError(ContainSubstring("generation")))
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
