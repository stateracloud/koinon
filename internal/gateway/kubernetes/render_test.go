// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
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
					{Name: "deepseek/deepseek-v4.1-flash", Guard: true, Price: &gateway.Price{Input: "0.30", Output: "1.20", CacheRead: "0.006", Tiers: []gateway.PriceTier{{ContextOver: 200000, Input: "0.60"}}}},
					{Name: "unpriced/model"},
				},
			},
			{
				Org: "globex", Name: "local", Type: "ollama", BaseURL: "http://ollama.globex.internal:11434/v1",
				Models: []gateway.Model{{Name: "deepseek/deepseek-v4.1-flash", Price: &gateway.Price{Input: "0", Output: "0"}}},
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
	s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 4.25, Window: 24 * time.Hour, Generation: "1792627200-750000000"})
	s.SetLimit(agentB, &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: 24 * time.Hour, Generation: "failed"})
	s.GrantTools(agentA,
		gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request"}},
		gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"read_page"}},
		// Another org's server: the rule is left out.
		gateway.ToolGrant{Target: "globex-search", Tools: []string{"query"}},
	)
	return s
}

func opts() gwk8s.Options {
	return gwk8s.Options{
		Namespace: "koinon", ProviderKeysSecret: "koinon-gateway-providers",
		AccessLogOTLP: &standalone.OTLPExport{HostPort: "koinon-api.koinon.svc:4318"},
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
	It("matches the golden file", func() {
		r, err := gwk8s.Render(fullState(), opts())
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join("testdata", "full.yaml")
		if *update {
			Expect(os.WriteFile(path, dump(r), 0o644)).To(Succeed()) //nolint:gosec // golden file
		}
		want, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(dump(r))).To(Equal(string(want)))
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
		s.SetLimit(agentB, &gateway.Limit{Unit: gateway.USD, Amount: 1, Window: 24 * time.Hour})
		again, err := gwk8s.Render(s, opts())
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Revoked).To(BeEmpty())
		Expect(find(again, "ConfigMap", "koinon-gateway-keys-acme").U.Object["data"]).To(HaveKey("k-bbbbbbbbbbbb"))
		Expect(again.Revision).NotTo(Equal(r.Revision))
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
				Models: []gateway.Model{{Name: "m", Price: &gateway.Price{Input: "1", Output: "1"}}},
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
			Expect(policy).To(ConsistOf(`apiKey.org == "` + m.Org + `" && !apiKey.internal`))
		}
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
		Expect(string(dump(r))).NotTo(ContainSubstring("kgw_"))
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
