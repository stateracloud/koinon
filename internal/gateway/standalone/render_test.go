// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
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

	It("logs who answered, with no \"$\" the config loader would expand (ADR 0056 amendment 2026-10-29)", func() {
		f := fixtures()[2]
		got, _, err := standalone.Render(f.state, f.options)
		Expect(err).NotTo(HaveOccurred())
		var doc struct {
			Frontend struct {
				AccessLog struct {
					Add map[string]string `yaml:"add"`
				} `yaml:"accessLog"`
			} `yaml:"frontendPolicies"`
		}
		Expect(yaml.Unmarshal(got, &doc)).To(Succeed())
		add := doc.Frontend.AccessLog.Add
		Expect(add).To(HaveKeyWithValue(agentgateway.LogFieldUpstream, agentgateway.UpstreamCEL))
		for name, expr := range add {
			Expect(expr).NotTo(ContainSubstring("$"), name+": agentgateway expands $ in its config file as an environment variable")
		}
	})

	// agentgateway v1.7.0 detects a reference to response.body or response.bodyPrefix from the
	// expression TEXT (crates/policy-core/src/expression.rs, telemetry/log.rs) and then records the
	// body of EVERY response on the listener (cel/mod.rs, called for each response in
	// proxy/httpproxy.rs), whatever its status and however the expression is guarded: up to 32 MiB
	// per LLM response held for the whole stream, and 2 MiB per MCP SSE stream for its lifetime.
	// Koinon's gateway has 512Mi and was designed to buffer nothing (ADR 0056 amendment 2026-10-29),
	// so no rendered log or tracer expression may read a response body.
	It("renders no expression that reads a response body", func() {
		for _, f := range fixtures() {
			got, _, err := standalone.Render(f.state, f.options)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).NotTo(ContainSubstring("response.body"), f.name)
		}
	})

	It("is deterministic and independent of key order", func() {
		f := fixtures()[2]
		a, revA, err := standalone.Render(f.state, f.options)
		Expect(err).NotTo(HaveOccurred())

		s := f.state
		s.Keys = slices.Clone(s.Keys)
		slices.Reverse(s.Keys)
		s.Providers = slices.Clone(s.Providers)
		slices.Reverse(s.Providers)
		for i := range s.Providers {
			s.Providers[i].Models = slices.Clone(s.Providers[i].Models)
			slices.Reverse(s.Providers[i].Models)
		}
		b, revB, err := standalone.Render(s, f.options)
		Expect(err).NotTo(HaveOccurred())
		Expect(b).To(Equal(a))
		Expect(revB).To(Equal(revA))
	})

	Describe("org-scoped models (plan item PV1)", func() {
		type apiKey struct {
			Metadata      struct{ Name, Org string } `yaml:"metadata"`
			AllowedModels *[]string                  `yaml:"allowedModels"`
		}
		type model struct {
			ID, Name string
			Matches  []struct {
				Headers []struct {
					Name  string
					Value struct{ Exact string }
				}
			}
			Overrides map[string]any
		}
		type doc struct {
			LLM struct {
				Policies struct {
					APIKey struct {
						Keys []apiKey `yaml:"keys"`
					} `yaml:"apiKey"`
					Transformations struct {
						Request struct {
							Replace string
							Set     map[string]string
						}
					}
				}
				Models []model
			}
		}
		render := func() doc {
			GinkgoHelper()
			out, _, err := standalone.Render(fixtures()[2].state, fixtures()[2].options)
			Expect(err).NotTo(HaveOccurred())
			var d doc
			Expect(yaml.Unmarshal(out, &d)).To(Succeed())
			return d
		}

		It("serves each model as <org>/<name>, matched on the org header", func() {
			d := render()
			var ids []string
			for _, m := range d.LLM.Models {
				ids = append(ids, m.ID)
				Expect(m.Matches).To(HaveLen(1))
				Expect(m.Matches[0].Headers).To(HaveLen(1))
				Expect(m.Matches[0].Headers[0].Name).To(Equal(gateway.OrgHeader))
				org, _, _ := strings.Cut(m.ID, "/")
				Expect(m.Matches[0].Headers[0].Value.Exact).To(Equal(org))
			}
			Expect(ids).To(ConsistOf("acme/deepseek/deepseek-v4.1-flash", "acme/open/model", "globex/deepseek/deepseek-v4.1-flash"))
		})

		It("stamps the key's org into the header before a model is chosen", func() {
			Expect(render().LLM.Policies.Transformations.Request.Set).To(HaveKeyWithValue(gateway.OrgHeader, `default(apiKey.org, "")`))
		})

		It("keeps only the allowlisted client headers on LLM and MCP routes (old issue 262)", func() {
			d := render()
			Expect(d.LLM.Policies.Transformations.Request.Replace).To(Equal(
				`request.headers.filterKeys(k, k in ["accept", "content-length", "content-type", "user-agent"])`))
			Expect(d.LLM.Policies.Transformations.Request.Set).To(HaveKey(gateway.OrgHeader), "the org header is set after the replace")

			got, _, err := standalone.Render(fixtures()[2].state, fixtures()[2].options)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(ContainSubstring(
				`replace: request.headers.filterKeys(k, k in ["accept", "content-length", "content-type", "last-event-id", "mcp-protocol-version", "mcp-session-id", "user-agent"])`))
			Expect(string(got)).To(ContainSubstring(
				"    authorization:\n      rules:\n        - '!toJson(request.headers.filterKeys(k, k != \"authorization\")).contains(\"kgw_\")'\n"),
				"an MCP request with a key in a header but authorization is refused: the allowlisted headers carry what the client chose")
		})

		It("limits every key to its own org's plain model names", func() {
			byName := map[string]apiKey{}
			for _, k := range render().LLM.Policies.APIKey.Keys {
				byName[k.Metadata.Name] = k
			}
			Expect(*byName["k-aaaaaaaaaaaa"].AllowedModels).To(Equal([]string{"deepseek/deepseek-v4.1-flash", "open/model"}))
			Expect(*byName["k-gggggggggggg"].AllowedModels).To(Equal([]string{"deepseek/deepseek-v4.1-flash"}))
			Expect(byName["k-gggggggggggg"].Metadata.Org).To(Equal("globex"))
			// The probe key is refused before routing; it has no list to be checked against.
			Expect(byName[gateway.ReloadProbeKeyName].AllowedModels).To(BeNil())
		})

		It("limits the model-check key to the models State.Models names, and a restricted agent likewise", func() {
			check := gateway.AgentRef{Org: "acme", Name: gateway.ModelCheckAgentName}
			s := fixtures()[2].state
			s.AddKey(fixedKey("k-cccccccccccc", check, "test-c"))
			s.RestrictModels(check, "open/model", "unserved/model")
			s.RestrictModels(gateway.AgentRef{Org: "acme", Name: "narrow"}, "open/model")
			s.AddKey(fixedKey("k-dddddddddddd", gateway.AgentRef{Org: "acme", Name: "narrow"}, "test-d"))
			out, _, err := standalone.Render(s, fixtures()[2].options)
			Expect(err).NotTo(HaveOccurred())
			var d struct {
				LLM struct {
					Policies struct {
						APIKey struct {
							Keys []apiKey `yaml:"keys"`
						} `yaml:"apiKey"`
					} `yaml:"policies"`
				} `yaml:"llm"`
			}
			Expect(yaml.Unmarshal(out, &d)).To(Succeed())
			byName := map[string]apiKey{}
			for _, k := range d.LLM.Policies.APIKey.Keys {
				byName[k.Metadata.Name] = k
			}
			Expect(*byName["k-cccccccccccc"].AllowedModels).To(Equal([]string{"open/model"}))
			Expect(*byName["k-dddddddddddd"].AllowedModels).To(Equal([]string{"open/model"}))
			Expect(*byName["k-aaaaaaaaaaaa"].AllowedModels).To(ContainElement("deepseek/deepseek-v4.1-flash"))

			By("a model-check key without an entry may request nothing")
			s.RestrictModels(check)
			out, _, err = standalone.Render(s, fixtures()[2].options)
			Expect(err).NotTo(HaveOccurred())
			Expect(yaml.Unmarshal(out, &d)).To(Succeed())
			for _, k := range d.LLM.Policies.APIKey.Keys {
				if k.Metadata.Name == "k-cccccccccccc" {
					Expect(*k.AllowedModels).To(BeEmpty())
				}
			}
		})

		It("denies every model to a key of an org that serves none", func() {
			s := gateway.State{Providers: fixtures()[2].state.Providers}
			s.AddKey(fixedKey("k-hhhhhhhhhhhh", gateway.AgentRef{Org: "initech", Name: "x"}, "test-h"))
			out, _, err := standalone.Render(s, standalone.Options{})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(out)).To(ContainSubstring("allowedModels: []"))
		})

		It("forces the routing guard only into guarded models", func() {
			for _, m := range render().LLM.Models {
				if m.ID == "acme/deepseek/deepseek-v4.1-flash" {
					Expect(m.Overrides).To(HaveKey("provider"))
				} else {
					Expect(m.Overrides).To(BeEmpty(), m.ID)
				}
			}
		})

		It("renders a model in its format (ADR 0069)", func() {
			f := fixtures()[5]
			Expect(f.name).To(Equal("model-formats"))
			out, _, err := standalone.Render(f.state, f.options)
			Expect(err).NotTo(HaveOccurred())
			var d struct {
				LLM struct {
					Models []struct {
						ID       string
						Provider struct {
							Custom struct {
								ProviderOverride string `yaml:"providerOverride"`
								Formats          []struct{ Type string }
							}
						}
						RequestHeaders struct {
							Set    map[string]string
							Remove []string
						} `yaml:"requestHeaders"`
						Transformation map[string]string
					}
				}
			}
			Expect(yaml.Unmarshal(out, &d)).To(Succeed())
			formats := map[string]string{}
			for _, m := range d.LLM.Models {
				formats[m.ID] = m.Provider.Custom.Formats[0].Type
				if m.ID == "acme/claude-haiku-4-5" {
					Expect(m.Transformation).To(And(HaveLen(1), HaveKey("tools")),
						"no stream_options; the agent's server tools are dropped")
					Expect(m.RequestHeaders.Remove).To(Equal([]string{"anthropic-beta"}))
					Expect(m.RequestHeaders.Set).To(Equal(map[string]string{
						"anthropic-version": "2023-06-01", "anthropic-workspace-id": "wrkspc_01ExampleExample",
					}))
					Expect(m.Provider.Custom.ProviderOverride).To(Equal("acme.anthropic"))
					continue
				}
				if m.ID == "acme/claude-haiku-5-5" {
					Expect(m.RequestHeaders.Remove).To(BeEmpty(), "the pinned beta replaces the client's")
					Expect(m.RequestHeaders.Set).To(Equal(map[string]string{
						"anthropic-version": "2023-06-01", "anthropic-workspace-id": "wrkspc_01ExampleExample",
						"anthropic-beta": thinking.BindingBeta,
					}))
					continue
				}
				Expect(m.Transformation).To(HaveKey("stream_options"), m.ID)
				Expect(m.RequestHeaders.Set).To(BeEmpty(), m.ID)
			}
			Expect(formats).To(HaveKeyWithValue("acme/claude-haiku-4-5", "messages"))
			Expect(formats).To(HaveKeyWithValue("acme/open/model", "completions"))
		})

		It("rejects the same model served twice in one org", func() {
			s := gateway.State{Providers: []gateway.Provider{
				{Org: "acme", Name: "a", Type: "x", BaseURL: "http://a/v1", Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{}}}},
				{Org: "acme", Name: "b", Type: "x", BaseURL: "http://b/v1", Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{}}}},
			}}
			_, _, err := standalone.Render(s, standalone.Options{})
			Expect(err).To(MatchError(ContainSubstring("served twice")))
		})
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
		Expect(fixtures()[2].state.Unpriced()).To(Equal([]string{"acme/unpriced/model"}))
		Expect(string(out)).NotTo(ContainSubstring("unpriced/model"))

		onlyUnpriced := gateway.State{Providers: []gateway.Provider{{
			Org: "acme", Name: "x", Type: "x", BaseURL: "http://x/v1", Models: []gateway.Model{{Name: "free", Format: modelformat.OpenAI}},
		}}}
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
				Expect(d.LLM.Policies.Authorization.Rules).To(Equal([]string{`apiKey.name != "koinon-reload-probe" && !url.decode(request.pathAndQuery).contains("kgw_") && !toJson(request.headers.filterKeys(k, k != "authorization")).contains("kgw_") && !has(llmRequest.mcp_servers)`}), "the probe is refused, and so are a key in the URL or a header and mcp_servers")
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

	It("gives a lone org target a placeholder that cannot collide with a server name", func() {
		s := gateway.State{MCP: []gateway.MCPTarget{{Name: "koinon-placeholder", Org: "koinon", URL: "http://k.example.com/mcp"}}}
		body, _, err := standalone.Render(s, standalone.Options{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(ContainSubstring("name: koinon.placeholder"))
		Expect(strings.Count(string(body), "name: koinon-placeholder")).To(Equal(1))
	})

	It("renders a limit with a window, and one without", func() {
		s := gateway.State{}
		s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: 24 * time.Hour})
		o := standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"}
		a, _, err := standalone.Render(s, o)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(a)).To(ContainSubstring("rolling: 24h"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1")})
		b, _, err := standalone.Render(s, o)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Count(string(b), "rolling:")).To(Equal(1), "only the reload probe keeps a window")
	})

	It("renders every USD limit fixed-point with at most nine fraction digits, rounded down, never an exponent (old issue 245)", func() {
		o := standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"}
		for _, c := range []struct{ spent, want string }{
			{"0.0172757592", "4.98272424"},    // a DeepSeek charge: ten digits
			{"0.000000123456", "4.999999876"}, // a twelve-digit reserve
			{"4.99995", "0.00005"},            // float64 prints 5e-05
			{"0.0000000004", "4.999999999"},   // below a nano: rounded down
			{"5", "0.0"},
		} {
			s := gateway.State{}
			s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
			l := gateway.USDLimit(money.MustParse("5").SubFloor(money.MustParse(c.spent)), 24*time.Hour, "g1")
			s.SetLimit(agentA, &l)
			body, _, err := standalone.Render(s, o)
			Expect(err).NotTo(HaveOccurred(), c.spent)
			var doc struct {
				LLM struct {
					Policies struct {
						APIKey struct {
							Keys []struct {
								Budgets []struct {
									Limit struct {
										Unit   string `yaml:"unit"`
										Amount string `yaml:"amount"`
									} `yaml:"limit"`
								} `yaml:"budgets"`
							} `yaml:"keys"`
						} `yaml:"apiKey"`
					} `yaml:"policies"`
				} `yaml:"llm"`
			}
			Expect(yaml.Unmarshal(body, &doc)).To(Succeed())
			var usd []string
			for _, k := range doc.LLM.Policies.APIKey.Keys {
				for _, b := range k.Budgets {
					if b.Limit.Unit == "USD" {
						usd = append(usd, b.Limit.Amount)
					}
				}
			}
			Expect(usd).To(HaveLen(1))
			Expect(usd[0]).To(MatchRegexp(`^\d+\.\d{1,9}$`))
			Expect(usd[0]).NotTo(ContainSubstring("e"))
			Expect(usd[0]).To(Equal(c.want), "spent %s", c.spent)
			Expect(money.MustParse(usd[0])).To(BeNumerically("<=", money.MustParse("5").SubFloor(money.MustParse(c.spent))), "never more than the remainder")
		}
	})

	It("refuses an unrounded amount instead of writing one agentgateway would reject", func() {
		s := gateway.State{}
		s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("4.9827242408"), Window: 24 * time.Hour})
		_, _, err := standalone.Render(s, standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"})
		Expect(err).To(MatchError(ContainSubstring("fraction digits")))
	})

	It("names the budget after the limit's generation", func() {
		s := gateway.State{Providers: fixtures()[2].state.Providers}
		s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: 24 * time.Hour, Generation: "g1"})
		o := standalone.Options{DatabaseURL: "sqlite:///tmp/x.db"}
		a, _, err := standalone.Render(s, o)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(a)).To(ContainSubstring("name: koinon-agent-limit-g1\n"))

		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: 24 * time.Hour, Generation: "Bad_Gen"})
		_, _, err = standalone.Render(s, o)
		Expect(err).To(MatchError(ContainSubstring("generation")))
	})

	It("requires a database for limits", func() {
		s := gateway.State{}
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: time.Hour})
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

// The MCP leak guard (ADR 0091) sits on every MCP call as a remote processor that fails closed;
// without a guard address the gateway refuses every call to an org's server instead.
var _ = Describe("Render the MCP leak guard", func() {
	type processor struct {
		Kind          string            `yaml:"kind"`
		Methods       map[string]string `yaml:"methods"`
		Host          string            `yaml:"host"`
		FailureMode   string            `yaml:"failureMode"`
		Metadata      map[string]string `yaml:"metadata"`
		Condition     string            `yaml:"condition"`
		Reject        string            `yaml:"reject"`
		RequestHeader struct {
			Allowed    []string `yaml:"allowed"`
			Disallowed []string `yaml:"disallowed"`
		} `yaml:"requestHeaders"`
	}
	processors := func(f fixture) []processor {
		got, _, err := standalone.Render(f.state, f.options)
		Expect(err).NotTo(HaveOccurred())
		var doc struct {
			MCP struct {
				Policies struct {
					Guardrails struct {
						Processors []processor `yaml:"processors"`
					} `yaml:"mcpGuardrails"`
				} `yaml:"policies"`
			} `yaml:"mcp"`
		}
		Expect(yaml.Unmarshal(got, &doc)).To(Succeed())
		return doc.MCP.Policies.Guardrails.Processors
	}
	byName := func(name string) fixture {
		for _, f := range fixtures() {
			if f.name == name {
				return f
			}
		}
		Fail("no fixture " + name)
		return fixture{}
	}

	It("sends every tools/call to the guard in both phases and fails closed", func() {
		f := byName("mcp-servers")
		f.options.GuardURL = "http://koinon-api-guard.koinon.svc:8083"
		ps := processors(f)
		Expect(ps).To(HaveLen(1))
		p := ps[0]
		Expect(p.Kind).To(Equal("remote"))
		Expect(p.Host).To(Equal("koinon-api-guard.koinon.svc:8083"))
		Expect(p.FailureMode).To(Equal("failClosed"))
		Expect(p.Methods).To(Equal(map[string]string{gateway.GuardMethod: "full"}))
		Expect(p.Metadata).To(Equal(gateway.GuardMetadata()))
		Expect(p.RequestHeader.Allowed).To(Equal(gateway.GuardAllowedHeaders))
		Expect(p.RequestHeader.Disallowed).To(ContainElements("authorization", "x-api-key"))
	})

	It("refuses every call to an org's server when no guard is configured", func() {
		ps := processors(byName("mcp-servers"))
		Expect(ps).To(HaveLen(1))
		Expect(ps[0].Kind).To(Equal("expression"))
		Expect(ps[0].Methods).To(Equal(map[string]string{gateway.GuardMethod: "full"}))
		Expect(ps[0].Condition).To(Equal(`mcp.tool.target in ["acme-legacy", "acme-wiki", "globex-search"]`))
		Expect(ps[0].Reject).To(Equal(gateway.GuardUnconfiguredReason))
	})

	DescribeTable("refuses a guard address agentgateway would not accept",
		func(raw string) {
			f := byName("mcp-servers")
			f.options.GuardURL = raw
			_, _, err := standalone.Render(f.state, f.options)
			Expect(err).To(HaveOccurred())
		},
		Entry("no port", "http://koinon-api-guard"),
		Entry("a path", "http://koinon-api-guard:8083/check"),
		Entry("another scheme", "grpc://koinon-api-guard:8083"),
		Entry("credentials", "http://u:p@koinon-api-guard:8083"),
	)
})
