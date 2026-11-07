// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
)

// update rewrites golden files: go test ./internal/gateway/kubernetes -args -update
var update = flag.Bool("update", false, "rewrite golden files")

var (
	agentA = gateway.AgentRef{Org: "acme", Name: "repo-helper"}
	agentB = gateway.AgentRef{Org: "acme", Name: "triage"}
	agentG = gateway.AgentRef{Org: "globex", Name: "analyst"}
)

// fixedKey: the hashes are of no real key.
func fixedKey(id string, agent gateway.AgentRef, material string) gateway.Key {
	return gateway.Key{ID: id, Agent: agent, Hash: gateway.HashKey(material)}
}

func fullState() gateway.State {
	s := gateway.State{
		Providers: []gateway.Provider{
			{
				Org: "acme", Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
				APIKeyFile: "/secrets/providers/acme-openrouter",
				Models: []gateway.Model{
					{Name: "deepseek/deepseek-v4.1-flash", Format: modelformat.OpenAI, Guard: true, Price: &gateway.Price{Input: "0.30", Output: "1.20", CacheRead: "0.006", Tiers: []gateway.PriceTier{{ContextOver: 200000, Input: "0.60"}}}},
					{Name: "unpriced/model", Format: modelformat.OpenAI},
				},
			},
			{
				Org: "globex", Name: "local", Type: "ollama", BaseURL: "http://ollama.globex.internal:11434/v1",
				Models: []gateway.Model{{Name: "deepseek/deepseek-v4.1-flash", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "0", Output: "0"}}},
			},
		},
		MCP: []gateway.MCPTarget{
			{Name: "github", URL: "http://koinon-github-tools.koinon.svc:8080/mcp"},
			{Name: "acme-wiki", Org: "acme", URL: "https://mcp.wiki.example.com/mcp", CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: "/secrets/providers/acme-wiki"},
			{Name: "globex-search", Org: "globex", URL: "https://search.example.org:8443/sse", SSE: true, CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/providers/globex-search"},
		},
	}
	s.AddKey(fixedKey("k-bbbbbbbbbbbb", agentB, "test-b"))
	s.AddKey(fixedKey("k-gggggggggggg", agentG, "test-g"))
	s.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
	s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("4.25"), Window: 24 * time.Hour, Generation: "1792627200-750000000"})
	s.SetLimit(agentB, &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: 24 * time.Hour, Generation: "failed"})
	s.GrantTools(agentA,
		gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request"}},
		gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"read_page"}},
		// Another org's server: the rule is left out.
		gateway.ToolGrant{Target: "globex-search", Tools: []string{"query"}},
	)
	return s
}

// formatsState is ADR 0069: one org with a model of each format, the Anthropic one on a
// workspace-scoped provider.
func formatsState() gateway.State {
	s := gateway.State{Providers: []gateway.Provider{
		{
			Org: "acme", Name: "claude", Type: "anthropic", BaseURL: "https://api.anthropic.com/v1",
			APIKeyFile: "/secrets/providers/acme-claude", AnthropicWorkspaceID: "wrkspc_01ExampleExample",
			Models: []gateway.Model{
				{Name: "claude-haiku-4-5", Format: modelformat.Anthropic, Price: &gateway.Price{Input: "1", Output: "5", CacheRead: "0.1", CacheWrite: "1.25"}},
				// A model whose thinking row pins a beta: the gateway sets it. No catalog row pins one
				// since the explicit forms of old issue 316, so the capability is spelled out here.
				{Name: "claude-haiku-5-5", Format: modelformat.Anthropic, Thinking: thinking.Capability{
					Thinks: thinking.Always, On: thinking.AdaptiveSummarized, Betas: []string{thinking.BindingBeta},
				}, Price: &gateway.Price{Input: "1", Output: "5", CacheRead: "0.1", CacheWrite: "1.25"}},
			},
		},
		{
			Org: "acme", Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
			APIKeyFile: "/secrets/providers/acme-openrouter",
			Models:     []gateway.Model{{Name: "open/model", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "2"}}},
		},
	}}
	s.AddKey(fixedKey("k-eeeeeeeeeeee", agentA, "test-e"))
	return s
}

func opts() gwk8s.Options {
	return gwk8s.Options{
		Namespace: "koinon", ProviderKeysSecret: "koinon-gateway-providers",
		AccessLogOTLP: &standalone.OTLPExport{HostPort: "koinon-api.koinon.svc:4318", TokenSecret: "koinon-otlp-token", TokenSecretKey: "token"},
		GuardURL:      "http://koinon-api-guard.koinon.svc:8083",
	}
}

func dump(r gwk8s.Rendered) []byte {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, o := range r.Objects {
		Expect(enc.Encode(o.U.Object)).To(Succeed()) // the encoder separates documents with ---
	}
	Expect(enc.Close()).To(Succeed())
	return append([]byte("# Revision "+string(r.Revision)+"\n"), buf.Bytes()...)
}

var _ = Describe("Render", func() {
	golden := func(s gateway.State, name string) {
		GinkgoHelper()
		r, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join("testdata", name+".yaml")
		if *update {
			Expect(os.WriteFile(path, dump(r), 0o644)).To(Succeed())
		}
		want, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(dump(r))).To(Equal(string(want)))
	}

	It("refuses a malformed OTLP receiver when a token Secret is set, instead of exporting without the token", func() {
		for _, bad := range []string{"http://koinon-api.koinon.svc:4318", "koinon-api.koinon.svc", "koinon-api.koinon.svc:http", ":4318"} {
			o := opts()
			o.AccessLogOTLP = &standalone.OTLPExport{HostPort: bad, TokenSecret: "koinon-otlp-token", TokenSecretKey: "token"}
			_, err := gwk8s.Render(fullState(), o)
			Expect(err).To(MatchError(ContainSubstring("OTLP receiver")), bad)
		}
	})

	It("matches the golden file", func() { golden(fullState(), "full") })
	It("matches the golden file for models in each format (ADR 0069)", func() { golden(formatsState(), "model-formats") })

	It("logs who answered (ADR 0056 amendment 2026-10-29)", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(dump(r))).To(ContainSubstring("name: " + agentgateway.LogFieldUpstream))
	})

	// agentgateway v1.7.0 records the body of EVERY response on a listener (up to 32 MiB per LLM
	// response, held for the whole stream) as soon as any log or tracer expression mentions
	// response.body or response.bodyPrefix, detected from the expression text and not narrowed by a
	// status guard. Koinon's gateway has 512Mi and buffers nothing (ADR 0056 amendment 2026-10-29).
	It("renders no expression that reads a response body", func() {
		for name, s := range map[string]gateway.State{"full": fullState(), "model-formats": formatsState()} {
			r, err := gwk8s.Render(s, opts())
			Expect(err).NotTo(HaveOccurred())
			Expect(string(dump(r))).NotTo(ContainSubstring("response.body"), name)
		}
	})

	It("is deterministic and independent of input order", func() {
		a, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		s := fullState()
		s.Keys[0], s.Keys[2] = s.Keys[2], s.Keys[0]
		s.Providers[0], s.Providers[1] = s.Providers[1], s.Providers[0]
		b, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Revision).To(Equal(a.Revision))
		Expect(dump(b)).To(Equal(dump(a)))
	})

	find := func(r gwk8s.Rendered, kind, name string) gwk8s.Object {
		GinkgoHelper()
		for _, o := range r.Objects {
			if o.U.GetKind() == kind && o.U.GetName() == name {
				return o
			}
		}
		Fail("no " + kind + "/" + name)
		return gwk8s.Object{}
	}

	It("leaves the keys of an exhausted limit out, and puts them back", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Revoked).To(Equal([]string{"k-bbbbbbbbbbbb"}))
		cm := find(r, "ConfigMap", "koinon-gateway-keys-acme")
		Expect(cm.U.Object["data"]).To(HaveKey("k-aaaaaaaaaaaa"))
		Expect(cm.U.Object["data"]).NotTo(HaveKey("k-bbbbbbbbbbbb"))

		s := fullState()
		s.SetLimit(agentB, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1"), Window: 24 * time.Hour})
		again, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Revoked).To(BeEmpty())
		Expect(find(again, "ConfigMap", "koinon-gateway-keys-acme").U.Object["data"]).To(HaveKey("k-bbbbbbbbbbbb"))
		Expect(again.Revision).NotTo(Equal(r.Revision))
	})

	It("renders a model in its format: Messages with the Anthropic headers and no stream_options, completions with it (ADR 0069)", func() {
		r, err := gwk8s.Render(formatsState(), opts())
		Expect(err).NotTo(HaveOccurred())
		spec := func(match string) map[string]any {
			GinkgoHelper()
			for _, o := range r.Objects {
				if o.U.GetKind() != gwk8s.KindModel {
					continue
				}
				sp, _ := o.U.Object["spec"].(map[string]any)
				if sp["match"].(map[string]any)["model"] == match && o.Org == "acme" {
					return sp
				}
			}
			Fail("no model " + match)
			return nil
		}
		claude := spec("claude-haiku-4-5")
		Expect(claude["custom"]).To(HaveKeyWithValue("formats", []any{map[string]any{"type": "Messages"}}))
		pol := claude["policies"].(map[string]any)
		Expect(pol["transformations"]).To(ConsistOf(HaveKeyWithValue("field", "tools")),
			"no stream_options; the agent's server tools are dropped")
		Expect(pol["authorization"]).To(HaveKeyWithValue("policy", HaveKeyWithValue("matchExpressions", ConsistOf(
			`apiKey.org == "acme" && !apiKey.internal && (!default(apiKey.restricted, false) || "claude-haiku-4-5" in default(apiKey.models, [])) && !has(llmRequest.mcp_servers) && !url.decode(request.pathAndQuery).contains("kgw_") && !toJson(request.headers.filterKeys(k, k != "authorization")).contains("kgw_")`))),
			"mcp_servers is refused, and so is a gateway key in the URL or in a header but authorization (old issue 262)")
		Expect(pol["headers"]).To(Equal(map[string]any{"request": map[string]any{
			"set": []any{
				map[string]any{"name": "anthropic-version", "value": "2023-06-01"},
				map[string]any{"name": "anthropic-workspace-id", "value": "wrkspc_01ExampleExample"},
			},
			"remove": []any{"anthropic-beta"},
		}}))

		By("a model whose row pins a beta gets it set, replacing whatever the client sends")
		think := spec("claude-haiku-5-5")["policies"].(map[string]any)
		Expect(think["headers"]).To(Equal(map[string]any{"request": map[string]any{
			"set": []any{
				map[string]any{"name": "anthropic-version", "value": "2023-06-01"},
				map[string]any{"name": "anthropic-workspace-id", "value": "wrkspc_01ExampleExample"},
				map[string]any{"name": "anthropic-beta", "value": thinking.BindingBeta},
			},
		}}), "a set replaces the client's header, so nothing is removed")

		chat := spec("open/model")
		Expect(chat["custom"]).To(HaveKeyWithValue("formats", []any{map[string]any{"type": "Completions"}}))
		Expect(chat["policies"]).NotTo(HaveKey("headers"))
		Expect(chat["policies"].(map[string]any)["transformations"]).To(ContainElement(
			HaveKeyWithValue("field", "stream_options")))
	})

	It("sets no workspace header and drops the client's when the provider has no workspace ID", func() {
		s := formatsState()
		for i := range s.Providers {
			s.Providers[i].AnthropicWorkspaceID = ""
		}
		r, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		var found bool
		for _, o := range r.Objects {
			if o.U.GetKind() == gwk8s.KindModel {
				sp, _ := o.U.Object["spec"].(map[string]any)
				if sp["match"].(map[string]any)["model"] == "claude-haiku-4-5" && o.Org == "acme" {
					found = true
					Expect(sp["policies"].(map[string]any)["headers"]).To(Equal(map[string]any{"request": map[string]any{
						"set":    []any{map[string]any{"name": "anthropic-version", "value": "2023-06-01"}},
						"remove": []any{"anthropic-beta", "anthropic-workspace-id"},
					}}))
				}
			}
		}
		Expect(found).To(BeTrue())
	})

	It("gives the model-check key its own model list, and no other key one", func() {
		check := gateway.AgentRef{Org: "acme", Name: gateway.ModelCheckAgentName}
		s := fullState()
		s.AddKey(fixedKey("k-cccccccccccc", check, "test-check"))
		s.SetLimit(check, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.10"), Window: 24 * time.Hour, Generation: "d"})
		s.RestrictModels(check, "deepseek/deepseek-v4.1-flash", "not-served/model")
		r, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		data := find(r, "ConfigMap", "koinon-gateway-keys-acme").U.Object["data"].(map[string]any)
		var entry struct {
			Metadata map[string]any `json:"metadata"`
		}
		Expect(json.Unmarshal([]byte(data["k-cccccccccccc"].(string)), &entry)).To(Succeed())
		Expect(entry.Metadata).To(HaveKeyWithValue("restricted", true))
		Expect(entry.Metadata["models"]).To(Equal([]any{"deepseek/deepseek-v4.1-flash"}), "only served, priced models of its org")
		Expect(entry.Metadata).To(HaveKeyWithValue("internal", false), "it requests models, unlike the MCP agents")
		Expect(data["k-aaaaaaaaaaaa"]).NotTo(ContainSubstring("restricted"))

		By("without an entry it may request nothing")
		s.RestrictModels(check)
		r, err = gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		data = find(r, "ConfigMap", "koinon-gateway-keys-acme").U.Object["data"].(map[string]any)
		Expect(data["k-cccccccccccc"]).To(ContainSubstring(`"models":[]`))
	})

	It("renders an empty key ConfigMap when an org's last key is revoked, so that the revocation is an update", func() {
		s := fullState()
		s.SetLimit(agentG, &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: time.Hour})
		r, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		cm := find(r, "ConfigMap", "koinon-gateway-keys-globex")
		Expect(cm.U.Object["data"]).To(BeEmpty())

		// An org with neither keys nor limits has no ConfigMap (the driver prunes it).
		s = fullState()
		s.RevokeKey("k-gggggggggggg")
		r, err = gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		for _, o := range r.Objects {
			Expect(o.U.GetName()).NotTo(Equal("koinon-gateway-keys-globex"))
		}
	})

	It("never names two objects alike, whatever an org is called", func() {
		for _, slug := range []string{"probe", "catalog", "edge", "frontend", "mcp", "keys-probe", "koinon-gateway"} {
			s := fullState()
			a := gateway.AgentRef{Org: slug, Name: "x"}
			s.AddKey(fixedKey("k-xxxxxxxxxxxx", a, "test-x"))
			s.Providers = append(s.Providers, gateway.Provider{
				Org: slug, Name: "p", Type: "fake", BaseURL: "http://p.example.com/v1",
				Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			})
			r, err := gwk8s.Render(s, opts())
			Expect(err).NotTo(HaveOccurred(), slug)
			seen := map[string]string{}
			for _, o := range r.Objects {
				Expect(seen).NotTo(HaveKey(o.Ref()), "org %q: %s", slug, o.Ref())
				seen[o.Ref()] = o.Org
			}
			// The probe key's ConfigMap belongs to the platform, not to any org.
			Expect(seen).To(HaveKeyWithValue("ConfigMap/koinon-gateway-probe", ""))
			if slug == "probe" {
				Expect(seen).To(HaveKeyWithValue("ConfigMap/koinon-gateway-keys-probe", "probe"))
			}
		}
	})

	It("serves only priced models, each only to its own org", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		var models []gwk8s.Object
		for _, o := range r.Objects {
			if o.U.GetKind() == gwk8s.KindModel {
				models = append(models, o)
			}
		}
		Expect(models).To(HaveLen(2))
		for _, m := range models {
			policy, _, _ := nested(m.U.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
			Expect(policy).To(HaveLen(1))
			expr := policy[0].(string)
			Expect(expr).To(HavePrefix(`apiKey.org == "` + m.Org + `" && !apiKey.internal && `))
			// A key with a model list of its own must name the model; other keys need nothing.
			Expect(expr).To(ContainSubstring(`!default(apiKey.restricted, false) || "deepseek/deepseek-v4.1-flash" in default(apiKey.models, [])`))
		}
	})

	It("hides an org's MCP server from other orgs' keys with a target condition (agentgateway v1.7)", func() {
		targets := func(s gateway.State) []any {
			GinkgoHelper()
			r, err := gwk8s.Render(s, opts())
			Expect(err).NotTo(HaveOccurred())
			t, _, _ := nested(find(r, gwk8s.KindBackend, "koinon-gateway-mcp").U.Object, "spec", "mcp", "targets")
			return t
		}
		byName := map[string]map[string]any{}
		for _, t := range targets(fullState()) {
			m := t.(map[string]any)
			byName[m["name"].(string)] = m
		}
		Expect(byName["acme-wiki"]).To(HaveKeyWithValue("condition", `apiKey.org == "acme"`))
		Expect(byName["globex-search"]).To(HaveKeyWithValue("condition", `apiKey.org == "globex"`))
		Expect(byName["github"]).NotTo(HaveKey("condition"), "a platform server is for every key")

		// The CRD refuses a condition on a lone target: it gets a never-true partner.
		lone := gateway.State{MCP: []gateway.MCPTarget{{Name: "acme-only", Org: "acme", URL: "http://acme.example.com/mcp"}}}
		got := targets(lone)
		Expect(got).To(HaveLen(2))
		Expect(got[1]).To(HaveKeyWithValue("name", "koinon.placeholder"))
		Expect(got[1]).To(HaveKeyWithValue("condition", "false"))
		// An org "koinon" with a server "placeholder" is named koinon-placeholder: no collision.
		clash := targets(gateway.State{MCP: []gateway.MCPTarget{{Name: "koinon-placeholder", Org: "koinon", URL: "http://k.example.com/mcp"}}})
		names := []string{}
		for _, t := range clash {
			names = append(names, t.(map[string]any)["name"].(string))
		}
		Expect(names).To(ConsistOf("koinon-placeholder", "koinon.placeholder"))
		// A lone platform server needs no partner.
		Expect(targets(gateway.State{MCP: []gateway.MCPTarget{{Name: "github", URL: "http://g.example.com/mcp"}}})).To(HaveLen(1))
	})

	It("refuses an MCP request with a gateway key in a header but authorization (old issue 262)", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		spec := find(r, gwk8s.KindPolicy, "koinon-gateway-mcp").U.Object["spec"]
		Expect(spec).To(HaveKeyWithValue("traffic", HaveKeyWithValue("authorization", And(
			HaveKeyWithValue("action", "Require"),
			HaveKeyWithValue("policy", HaveKeyWithValue("matchExpressions", ConsistOf(
				`!toJson(request.headers.filterKeys(k, k != "authorization")).contains("kgw_")`)))))),
			"the allowlisted headers carry what the client chose, and this CRD cannot allowlist at all")
	})

	It("refuses a provider key without a Secret to hold it", func() {
		o := opts()
		o.ProviderKeysSecret = ""
		_, err := gwk8s.Render(fullState(), o)
		Expect(err).To(MatchError(ContainSubstring("ProviderKeysSecret")))
	})

	It("holds no key material, only hashes", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		// The admission rules name the prefix (gateway.NoKeyInURLExpr); a key is the prefix and 43 characters.
		Expect(string(dump(r))).NotTo(MatchRegexp(`kgw_[A-Za-z0-9_-]{43}`))
	})
})

func nested(obj map[string]any, path ...string) ([]any, bool, error) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur = m[p]
	}
	s, ok := cur.([]any)
	return s, ok, nil
}

// The MCP leak guard (ADR 0091): every org server sits behind the one MCP Backend, whose guardrails
// send each tools/call to koinon-api and fail closed; without a guard address they refuse instead.
var _ = Describe("Render the MCP leak guard", func() {
	guardrails := func(o gwk8s.Options) map[string]any {
		GinkgoHelper()
		r, err := gwk8s.Render(fullState(), o)
		Expect(err).NotTo(HaveOccurred())
		var found []map[string]any
		for _, obj := range r.Objects {
			if obj.U.GetKind() != gwk8s.KindBackend {
				continue
			}
			if _, mcp := obj.U.Object["spec"].(map[string]any)["mcp"]; !mcp {
				continue
			}
			spec := obj.U.Object["spec"].(map[string]any)
			targets := spec["mcp"].(map[string]any)["targets"].([]any)
			var orgs []string
			for _, t := range targets {
				if c, ok := t.(map[string]any)["condition"].(string); ok && c != "false" {
					orgs = append(orgs, c)
				}
			}
			Expect(orgs).NotTo(BeEmpty(), "the org servers are on this Backend")
			g, ok := spec["policies"].(map[string]any)["mcp"].(map[string]any)["guardrails"].(map[string]any)
			Expect(ok).To(BeTrue(), "the MCP Backend carries guardrails")
			found = append(found, g)
		}
		Expect(found).To(HaveLen(1))
		return found[0]
	}

	It("sends every tools/call to the guard in both phases and fails closed", func() {
		ps := guardrails(opts())["processors"].([]any)
		Expect(ps).To(HaveLen(1))
		p := ps[0].(map[string]any)
		Expect(p["methods"]).To(Equal(map[string]any{gateway.GuardMethod: "Full"}))
		remote := p["remote"].(map[string]any)
		Expect(remote["url"]).To(Equal("http://koinon-api-guard.koinon.svc:8083"))
		Expect(remote["failureMode"]).To(Equal("FailClosed"))
		Expect(remote["metadata"]).To(HaveKeyWithValue("agent", "apiKey.agent"))
		Expect(remote["metadata"]).To(HaveKeyWithValue("tool", "mcp.tool.name"))
		// An empty allow list would forward the agent's gateway key to the guard.
		Expect(remote["allowedRequestHeaders"]).To(Equal([]any{"mcp-protocol-version"}))
		Expect(remote["disallowedRequestHeaders"]).To(ContainElements("authorization", "x-api-key"))
	})

	It("refuses every call to an org's server when no guard is configured", func() {
		o := opts()
		o.GuardURL = ""
		ps := guardrails(o)["processors"].([]any)
		Expect(ps).To(HaveLen(1))
		p := ps[0].(map[string]any)
		Expect(p["methods"]).To(Equal(map[string]any{gateway.GuardMethod: "Full"}))
		Expect(p["expression"]).To(Equal(map[string]any{
			"condition": `mcp.tool.target in ["acme-wiki", "globex-search"]`,
			"reject":    gateway.GuardUnconfiguredReason,
		}))
	})

	It("refuses a guard address the CRD would not accept", func() {
		for _, raw := range []string{"http://koinon-api-guard", "http://koinon-api-guard:8083/check", "grpc://koinon-api-guard:8083"} {
			o := opts()
			o.GuardURL = raw
			_, err := gwk8s.Render(fullState(), o)
			Expect(err).To(HaveOccurred(), raw)
		}
	})
})
