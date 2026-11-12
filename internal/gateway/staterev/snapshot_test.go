// SPDX-License-Identifier: Apache-2.0

package staterev_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
)

func TestStaterev(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Staterev Suite")
}

var (
	agentA = gateway.AgentRef{Org: "acme", Name: "alpha"}
	agentB = gateway.AgentRef{Org: "acme", Name: "beta"}
	agentC = gateway.AgentRef{Org: "zeta", Name: "gamma"}
)

// richState has something in every field of State.
func richState() gateway.State {
	var s gateway.State
	// Fixed keys, so two calls build equal States.
	s.AddKey(gateway.Key{ID: "k-0a", Agent: agentA, Hash: gateway.HashKey("koinon_test_not-a-real-token-a")})
	s.AddKey(gateway.Key{ID: "k-0b", Agent: agentB, Hash: gateway.HashKey("koinon_test_not-a-real-token-b")})
	s.AddKey(gateway.Key{ID: "k-0c", Agent: agentC, Hash: gateway.HashKey("koinon_test_not-a-real-token-c")})
	s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1.5"), Window: 24 * time.Hour, Generation: "g1"})
	s.SetLimit(agentC, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.25")})
	s.Providers = []gateway.Provider{
		{Org: "zeta", Name: "p2", Type: "ollama", BaseURL: "http://ollama/v1", Models: []gateway.Model{
			{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "0", Output: "0"}},
		}},
		{Org: "acme", Name: "p1", Type: "anthropic", BaseURL: "http://a/v1", APIKeyFile: "/keys/acme-p1", AnthropicWorkspaceID: "wrkspc_01AbC", Models: []gateway.Model{
			{
				Name: "b", Format: modelformat.Anthropic, Guard: true,
				Thinking: thinking.Capability{Thinks: thinking.Optional, Betas: []string{"y", "x"}, Display: thinking.DisplaySummarized},
				Price: &gateway.Price{
					Input: "3", Output: "15", CacheRead: "0.3", CacheWrite: "3.75", Reasoning: "15",
					Tiers: []gateway.PriceTier{{ContextOver: 200000, Input: "6", Output: "22.5"}},
				},
			},
			{Name: "a", Format: modelformat.OpenAI},
		}},
	}
	s.RestrictModels(agentA, "b", "a")
	s.MCP = []gateway.MCPTarget{
		{Name: "wiki", URL: "http://wiki/mcp", Org: "acme", CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: "/creds/wiki"},
		{Name: "docs", URL: "http://docs/sse", SSE: true},
	}
	s.GrantTools(agentA, gateway.ToolGrant{Target: "wiki", Tools: []string{"search", "read"}}, gateway.ToolGrant{Target: "docs", All: true})
	return s
}

var _ = Describe("Snapshot", func() {
	It("round-trips every field of a State", func() {
		s := richState()
		u := staterev.Snapshot(s)
		back := staterev.FromProto(u)
		// A snapshot sorts its sets, so compare against the sorted State.
		again := staterev.Snapshot(back)
		Expect(proto.Equal(u, again)).To(BeTrue(), "snapshot of the decoded state differs")
		Expect(back.Limits).To(Equal(s.Limits))
		Expect(back.Keys).To(ConsistOf(s.Keys))
		Expect(back.MCP).To(ConsistOf(s.MCP))
		Expect(back.Providers).To(HaveLen(2))
		Expect(back.Providers[0].Models[1].Price.Tiers).To(Equal(s.Providers[1].Models[0].Price.Tiers))
	})

	It("is the same for the same State built in another order", func() {
		a := richState()
		b := richState()
		slices.Reverse(b.Keys)
		slices.Reverse(b.Providers)
		slices.Reverse(b.MCP)
		slices.Reverse(b.Providers[1].Models)
		b.Tools[agentA] = []gateway.ToolGrant{b.Tools[agentA][1], b.Tools[agentA][0]}
		Expect(proto.Equal(staterev.Snapshot(a), staterev.Snapshot(b))).To(BeTrue())
	})

	It("gives identical bytes for the same State with every input slice permuted", func() {
		a := richState()
		b := richState()
		b.Tools[agentA] = append(b.Tools[agentA], gateway.ToolGrant{Target: "wiki", Tools: []string{"zz", "aa"}})
		a.Tools[agentA] = append(a.Tools[agentA], gateway.ToolGrant{Target: "wiki", Tools: []string{"aa", "zz"}})
		a.Tools[agentA] = append([]gateway.ToolGrant{a.Tools[agentA][2]}, a.Tools[agentA][:2]...)
		a.Providers[1].Models[0].Price.Tiers = append(a.Providers[1].Models[0].Price.Tiers, gateway.PriceTier{ContextOver: 100000, Input: "4"})
		b.Providers[1].Models[0].Price.Tiers = []gateway.PriceTier{{ContextOver: 100000, Input: "4"}, b.Providers[1].Models[0].Price.Tiers[0]}
		slices.Reverse(b.Keys)
		slices.Reverse(b.Providers)
		b.Providers[0].Models[0], b.Providers[0].Models[1] = b.Providers[0].Models[1], b.Providers[0].Models[0]
		ba, err := proto.MarshalOptions{Deterministic: true}.Marshal(staterev.Snapshot(a))
		Expect(err).NotTo(HaveOccurred())
		bb, err := proto.MarshalOptions{Deterministic: true}.Marshal(staterev.Snapshot(b))
		Expect(err).NotTo(HaveOccurred())
		Expect(bb).To(Equal(ba))
	})

	It("differs when a limit or a grant changes", func() {
		a := richState()
		b := richState()
		b.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("1.6"), Window: 24 * time.Hour, Generation: "g1"})
		Expect(proto.Equal(staterev.Snapshot(a), staterev.Snapshot(b))).To(BeFalse())
	})
})

// The hard rule (#279 slice 2): a stored revision and a StateUpdate carry no secret material.
var _ = Describe("secret material", func() {
	// reviewedLeaves is every string, number and bool a State carries, reviewed as not secret on
	// 2026-11-12. Adding a field to State fails the first spec below until a person adds it here
	// (and to Snapshot, or consciously leaves it off the wire). The path-valued fields name where a
	// credential is mounted in the gateway, not the credential.
	reviewedLeaves := []string{
		"Keys[].Agent.Name", "Keys[].Agent.Org", "Keys[].Hash", "Keys[].ID",
		"Limits{}.Amount", "Limits{}.Generation", "Limits{}.Unit", "Limits{}.Window", "Limits{}~Name", "Limits{}~Org",
		"MCP[].CredentialFile", "MCP[].CredentialHeader", "MCP[].CredentialPrefix", "MCP[].Name", "MCP[].Org", "MCP[].SSE", "MCP[].URL",
		"Models{}[]", "Models{}~Name", "Models{}~Org",
		"Providers[].AnthropicWorkspaceID", "Providers[].APIKeyFile", "Providers[].BaseURL",
		"Providers[].Models[].Format", "Providers[].Models[].Guard", "Providers[].Models[].Name",
		"Providers[].Models[].Price.CacheRead", "Providers[].Models[].Price.CacheWrite", "Providers[].Models[].Price.Input",
		"Providers[].Models[].Price.Output", "Providers[].Models[].Price.Reasoning",
		"Providers[].Models[].Price.Tiers[].CacheRead", "Providers[].Models[].Price.Tiers[].CacheWrite",
		"Providers[].Models[].Price.Tiers[].ContextOver", "Providers[].Models[].Price.Tiers[].Input",
		"Providers[].Models[].Price.Tiers[].Output", "Providers[].Models[].Price.Tiers[].Reasoning",
		"Providers[].Models[].Thinking.Betas[]", "Providers[].Models[].Thinking.Display", "Providers[].Models[].Thinking.Off",
		"Providers[].Models[].Thinking.On", "Providers[].Models[].Thinking.Thinks", "Providers[].Models[].Thinking.UsageSplit",
		"Providers[].Name", "Providers[].Org", "Providers[].Type",
		"Tools{}[].All", "Tools{}[].Target", "Tools{}[].Tools[]", "Tools{}~Name", "Tools{}~Org",
	}

	It("lists every leaf field of State, so a new field is reviewed", func() {
		got := leafPaths(reflect.TypeOf(gateway.State{}))
		slices.Sort(got)
		want := slices.Clone(reviewedLeaves)
		slices.Sort(want)
		Expect(got).To(Equal(want), "gateway.State changed: review the new field for secret material, then update reviewedLeaves and Snapshot")
	})

	It("sends every reviewed leaf, and nothing else, over the wire", func() {
		var s gateway.State
		canaries := map[string]string{}
		fill(reflect.ValueOf(&s).Elem(), "", canaries)
		raw, err := protojson.Marshal(staterev.Snapshot(s))
		Expect(err).NotTo(HaveOccurred())
		text := string(raw)
		for canary, path := range canaries {
			Expect(text).To(ContainSubstring(canary), "State field %s is dropped by Snapshot", path)
		}
		// The converse: every string on the wire is a canary of a reviewed field or a constant.
		for _, quoted := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(text, -1) {
			v := quoted[1]
			if _, ok := canaries[v]; ok || isFieldName(v) || isNumber(v) {
				continue
			}
			Fail("unexpected value on the wire: " + v)
		}
	})

	It("keeps secret-like names out of the StateUpdate message", func() {
		secretish := regexp.MustCompile(`(?i)secret|password|passwd|token|material|api_?key$|private|bearer_|plaintext`)
		// Fields that name a mounted path or a header, not a value.
		allowed := map[string]bool{"api_key_file": true, "credential_header": true, "credential_prefix": true, "credential_file": true}
		var walk func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool)
		walk = func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
			if seen[md.FullName()] {
				return
			}
			seen[md.FullName()] = true
			for i := 0; i < md.Fields().Len(); i++ {
				f := md.Fields().Get(i)
				name := string(f.Name())
				if secretish.MatchString(name) && !allowed[name] {
					Fail(fmt.Sprintf("%s.%s looks like it carries a secret", md.FullName(), name))
				}
				if f.Message() != nil {
					walk(f.Message(), seen)
				}
			}
		}
		walk((&gwv1.StateUpdate{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})
	})

	It("never carries key material or the content of a provider key file", func() {
		// Obviously fake, low-entropy values.
		const providerKey = "koinon_test_not-a-real-token-provider-key"
		const mcpCredential = "koinon_test_not-a-real-token-mcp-credential"
		dir := GinkgoT().TempDir()
		pkFile := filepath.Join(dir, "provider-key")
		cFile := filepath.Join(dir, "mcp-credential")
		Expect(os.WriteFile(pkFile, []byte(providerKey), 0o600)).To(Succeed())
		Expect(os.WriteFile(cFile, []byte(mcpCredential), 0o600)).To(Succeed())

		issued, err := gateway.NewKey(agentA)
		Expect(err).NotTo(HaveOccurred())
		var s gateway.State
		s.AddKey(issued.Key)
		s.Providers = []gateway.Provider{{
			Org: "acme", Name: "p", Type: "openai", BaseURL: "http://p/v1", APIKeyFile: pkFile,
			Models: []gateway.Model{{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "2"}}},
		}}
		s.MCP = []gateway.MCPTarget{{Name: "wiki", URL: "http://wiki/mcp", CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: cFile}}

		u := staterev.Snapshot(s)
		wire, err := proto.Marshal(u)
		Expect(err).NotTo(HaveOccurred())
		js, err := protojson.Marshal(u)
		Expect(err).NotTo(HaveOccurred())
		for _, secret := range []string{issued.Material, providerKey, mcpCredential} {
			Expect(string(wire)).NotTo(ContainSubstring(secret))
			Expect(string(js)).NotTo(ContainSubstring(secret))
		}
		// What is there: the hash and the paths.
		Expect(string(js)).To(And(ContainSubstring(issued.Hash), ContainSubstring(pkFile), ContainSubstring(cFile)))
	})
})

// isFieldName: protojson with default options writes field names in lowerCamelCase; those are the
// only non-canary strings besides enum-free constants.
func isFieldName(s string) bool {
	return regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`).MatchString(s) && !strings.HasPrefix(s, "koinon_test")
}

func isNumber(s string) bool { return regexp.MustCompile(`^[0-9]+$`).MatchString(s) }

// leafPaths lists the leaf fields of t. "[]" is a slice, "{}" a map value, "{}~Field" a field of a
// map key.
func leafPaths(t reflect.Type) []string {
	var out []string
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		switch t.Kind() {
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				p := f.Name
				if path != "" && !f.Anonymous {
					p = path + "." + f.Name
				} else if path != "" {
					p = path
					walk(f.Type, p)
					continue
				}
				if f.Anonymous {
					walk(f.Type, "")
					continue
				}
				walk(f.Type, p)
			}
		case reflect.Slice:
			if t.Elem().Kind() == reflect.Uint8 {
				out = append(out, path)
				return
			}
			walk(t.Elem(), path+"[]")
		case reflect.Map:
			kt := t.Key()
			if kt.Kind() == reflect.Struct {
				for i := 0; i < kt.NumField(); i++ {
					out = append(out, path+"{}~"+kt.Field(i).Name)
				}
			} else {
				out = append(out, path+"{}~key")
			}
			walk(t.Elem(), path+"{}")
		case reflect.Pointer:
			walk(t.Elem(), path)
		default:
			out = append(out, path)
		}
	}
	walk(t, "")
	return out
}

// fill gives every string leaf of v a distinct canary, records canary -> path, and gives numbers
// and bools a non-zero value. Maps and slices get one element.
func fill(v reflect.Value, path string, canaries map[string]string) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), path+"."+v.Type().Field(i).Name, canaries)
			}
		}
	case reflect.String:
		c := fmt.Sprintf("koinon_test_not-a-real-token-%d", len(canaries))
		v.SetString(c)
		canaries[c] = path
	case reflect.Int, reflect.Int64:
		v.SetInt(7 * int64(time.Second))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		el := reflect.New(v.Type().Elem()).Elem()
		fill(el, path+"[]", canaries)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), el))
	case reflect.Map:
		k := reflect.New(v.Type().Key()).Elem()
		fill(k, path+"~key", canaries)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, path+"{}", canaries)
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(k, e)
	case reflect.Pointer:
		n := reflect.New(v.Type().Elem())
		fill(n.Elem(), path, canaries)
		v.Set(n)
	}
}
