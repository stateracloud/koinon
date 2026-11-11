// SPDX-License-Identifier: Apache-2.0

package policycheck_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/testing/policycheck"
)

func TestPolicycheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "policycheck Suite")
}

func policy(ep *sandboxv1.NetworkEndpoint) *sandboxv1.SandboxPolicy {
	return &sandboxv1.SandboxPolicy{NetworkPolicies: map[string]*sandboxv1.NetworkPolicyRule{
		"r": {Endpoints: []*sandboxv1.NetworkEndpoint{ep}, Binaries: []*sandboxv1.NetworkBinary{{Path: "/usr/bin/curl"}}},
	}}
}

func rest(rules []*sandboxv1.L7Rule, deny []*sandboxv1.L7DenyRule) *sandboxv1.NetworkEndpoint {
	return &sandboxv1.NetworkEndpoint{
		Host: "api.example.com", Port: 443, Protocol: "rest",
		Enforcement: sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE,
		Rules:       rules, DenyRules: deny,
	}
}

func allow(method, path string) *sandboxv1.L7Rule {
	return &sandboxv1.L7Rule{Allow: &sandboxv1.L7Allow{Method: method, Path: path}}
}

func req(method, path, query string) policycheck.Request {
	return policycheck.Request{Binary: "/usr/bin/curl", Host: "API.example.com", Port: 443, Method: method, Path: path, Query: query}
}

var _ = Describe("Evaluate", func() {
	It("matches path globs as the schema reference describes", func() {
		p := policy(rest([]*sandboxv1.L7Rule{allow("GET", "/repos/**"), allow("GET", "/a/*.git/x")}, nil))
		for path, want := range map[string]bool{
			"/repos/o/r": true, "/repos/o": true, "/repos": false, "/a/r.git/x": true, "/a/b/r.git/x": false,
			"/Repos/o": false, "/repos/../admin": false,
		} {
			Expect(policycheck.Evaluate(p, nil, req("GET", path, "")).Allowed).To(Equal(want), path)
		}
		Expect(policycheck.Evaluate(p, nil, req("POST", "/repos/o", "")).Allowed).To(BeFalse())
	})

	It("lets deny rules win and matches their query parameters", func() {
		ep := rest([]*sandboxv1.L7Rule{{Allow: &sandboxv1.L7Allow{
			Method: "GET", Path: "/**/info/refs",
			Query: map[string]*sandboxv1.L7QueryMatcher{"service": {Glob: "git-upload-pack"}},
		}}}, []*sandboxv1.L7DenyRule{{
			Method: "GET", Path: "/**/info/refs",
			Query: map[string]*sandboxv1.L7QueryMatcher{"service": {Glob: "git-receive-pack"}},
		}})
		p := policy(ep)
		Expect(policycheck.Evaluate(p, nil, req("GET", "/o/r/info/refs", "service=git-upload-pack")).Allowed).To(BeTrue())
		Expect(policycheck.Evaluate(p, nil, req("GET", "/o/r/info/refs", "service=git-receive-pack")).Allowed).To(BeFalse())
		Expect(policycheck.Evaluate(p, nil, req("GET", "/o/r/info/refs", "service=git-upload-pack&service=git-receive-pack")).Allowed).To(BeFalse())
	})

	It("allows under audit, and nothing for other binaries or hosts", func() {
		ep := rest([]*sandboxv1.L7Rule{allow("GET", "/x")}, nil)
		p := policy(ep)
		other := req("GET", "/x", "")
		other.Binary = "/tmp/curl"
		Expect(policycheck.Evaluate(p, nil, other).Allowed).To(BeFalse())
		other = req("GET", "/x", "")
		other.Host = "evil.example.com"
		Expect(policycheck.Evaluate(p, nil, other).Allowed).To(BeFalse())
		ep.Enforcement = sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_AUDIT
		Expect(policycheck.Evaluate(p, nil, req("POST", "/y", "")).Allowed).To(BeTrue())
	})

	It("adds attached profiles as provider entries", func() {
		profile := &openshellv1.ProviderProfile{
			Id:        "p",
			Endpoints: []*sandboxv1.NetworkEndpoint{rest([]*sandboxv1.L7Rule{allow("GET", "/p")}, nil)},
			Binaries:  []*sandboxv1.NetworkBinary{{Path: "/usr/bin/curl"}},
		}
		Expect(policycheck.Evaluate(&sandboxv1.SandboxPolicy{}, nil, req("GET", "/p", "")).Allowed).To(BeFalse())
		Expect(policycheck.Evaluate(&sandboxv1.SandboxPolicy{}, []*openshellv1.ProviderProfile{profile}, req("GET", "/p", "")).Allowed).To(BeTrue())
	})
})
