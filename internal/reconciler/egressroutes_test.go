// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("The org egress routes of an agent (#1086 part B)", func() {
	ref := gateway.AgentRef{Org: "acme", Name: "00000000-0000-0000-0000-0000000000a1"}
	actor := actortoken.Actor{Org: "acme", Agent: ref.Name, Run: "r1"}
	var (
		minter *actortoken.Minter
		ver    *actortoken.Verifier
	)
	BeforeEach(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		minter, err = actortoken.NewMinter("koinon-test", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		ver, err = actortoken.NewVerifier("koinon-test", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
	})
	cfg := agentconfig.Config{
		Grants:   []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
		Bindings: []agentconfig.Binding{{Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000001", Targets: []string{"acme/widgets"}}},
	}

	table := func(routes []route.Route) *route.Gate {
		t, err := route.Compile(routes)
		Expect(err).NotTo(HaveOccurred())
		return route.NewGate(ver, t)
	}
	decide := func(g *route.Gate, a actortoken.Actor, method, host, target string) route.Decision {
		u, err := url.Parse(target)
		Expect(err).NotTo(HaveOccurred())
		tok, err := minter.Mint(a, 0)
		Expect(err).NotTo(HaveOccurred())
		return g.Check(route.Request{Scheme: "https", Token: tok, Authority: host, Method: method, Path: u.EscapedPath(), RawQuery: u.RawQuery})
	}

	It("translates the repo.read grant into forge routes with the agent's scope and credential name", func() {
		read, ok, err := reconciler.GitHubReadAccess(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		routes, err := reconciler.EgressRoutes(ref, read.Endpoints, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).NotTo(BeEmpty())
		for _, r := range routes {
			Expect(r.Class).To(Equal(route.ClassForge))
			Expect(r.Org).To(Equal("acme"))
			Expect(r.Agent).To(Equal(ref.Name))
			Expect(r.Name).To(HavePrefix("org/acme/agent/" + ref.Name + "/forge-"))
			if r.Deny {
				Expect(r.Credential).To(BeEmpty())
			} else {
				Expect(r.Credential).To(Equal(reconciler.CodeHostCredentialName(ref)))
				Expect(r.Credential).NotTo(ContainSubstring("koinon_")) // a name, never a value
			}
		}

		t := table(routes)
		// The grant's own requests are routed.
		Expect(decide(t, actor, "GET", "github.com", "/acme/widgets.git/info/refs?service=git-upload-pack").Allowed).To(BeTrue())
		Expect(decide(t, actor, "POST", "github.com", "/acme/widgets.git/git-upload-pack").Allowed).To(BeTrue())
		Expect(decide(t, actor, "GET", "api.github.com", "/repos/acme/widgets/pulls?page=2").Allowed).To(BeTrue())
		// Everything else is refused: another repository, a write, a push however it is spelled.
		Expect(decide(t, actor, "GET", "github.com", "/acme/other.git/info/refs?service=git-upload-pack").Allowed).To(BeFalse())
		Expect(decide(t, actor, "POST", "api.github.com", "/repos/acme/widgets/pulls").Allowed).To(BeFalse())
		Expect(decide(t, actor, "GET", "github.com", "/acme/widgets.git/info/refs?service=git-receive-pack").Allowed).To(BeFalse())
		Expect(decide(t, actor, "GET", "github.com", "/acme/widgets.git/info/refs?service=GIT-RECEIVE-PACK").Allowed).To(BeFalse())
		Expect(decide(t, actor, "POST", "github.com", "/acme/widgets.git/git-receive-pack").Allowed).To(BeFalse())
		Expect(decide(t, actor, "POST", "github.com", "/acme/widgets.git/GIT-RECEIVE-PACK").Allowed).To(BeFalse())
		Expect(decide(t, actor, "POST", "github.com", "/acme/widgets.git/git%2Dreceive-pack").Allowed).To(BeFalse())
	})

	It("serves the routes to the agent only, never to another agent or org", func() {
		read := mustRead(cfg)
		routes, err := reconciler.EgressRoutes(ref, read.Endpoints, true)
		Expect(err).NotTo(HaveOccurred())
		t := table(routes)
		other := actortoken.Actor{Org: "acme", Agent: "00000000-0000-0000-0000-0000000000b2", Run: "r2"}
		Expect(decide(t, other, "POST", "github.com", "/acme/widgets.git/git-upload-pack").Allowed).To(BeFalse())
		Expect(decide(t, other, "GET", "registry.npmjs.org", "/left-pad").Allowed).To(BeFalse())
		foreign := actortoken.Actor{Org: "globex", Agent: ref.Name, Run: "r3"}
		Expect(decide(t, foreign, "GET", "registry.npmjs.org", "/left-pad").Allowed).To(BeFalse())
	})

	It("adds the npm registry rule without a credential when plugin installs are on", func() {
		routes, err := reconciler.EgressRoutes(ref, nil, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(HaveLen(2))
		for _, r := range routes {
			Expect(r.Class).To(Equal(route.ClassRegistry))
			Expect(r.Credential).To(BeEmpty())
			Expect(r.Host).To(Equal(reconciler.NPMRegistryHost))
		}
		t := table(routes)
		Expect(decide(t, actor, "GET", "registry.npmjs.org", "/@scope/pkg").Allowed).To(BeTrue())
		// The route model refuses an encoded slash in any path (part A, fail closed), also the
		// npm CLI's spelling of a scoped package; that needs its own change before npm goes
		// through the gateway.
		Expect(decide(t, actor, "GET", "registry.npmjs.org", "/@scope%2fpkg").Reason).To(Equal(route.ReasonBadRequest))
		Expect(decide(t, actor, "HEAD", "registry.npmjs.org", "/left-pad/-/left-pad-1.3.0.tgz").Allowed).To(BeTrue())
		Expect(decide(t, actor, "PUT", "registry.npmjs.org", "/left-pad").Allowed).To(BeFalse())
	})

	It("gives no route without a grant and without plugin installs", func() {
		routes, err := reconciler.EgressRoutes(ref, nil, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(BeEmpty())
	})

	DescribeTable("refuses a rule the route model cannot express exactly",
		func(ep execution.Endpoint, want string) {
			_, err := reconciler.EgressRoutes(ref, []execution.Endpoint{ep}, false)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("an L4-only endpoint", execution.Endpoint{Host: "github.com", Port: 443}, "not an enforced REST endpoint"),
		Entry("an access preset", execution.Endpoint{
			Host: "github.com", Port: 443, Protocol: "rest", Access: execution.AccessReadOnly,
			Allow: []execution.L7Rule{{Method: "GET", Path: "/**"}},
		}, "access preset"),
		Entry("an endpoint path", execution.Endpoint{
			Host: "github.com", Port: 443, Protocol: "rest", Path: "/api/**",
			Allow: []execution.L7Rule{{Method: "GET", Path: "/**"}},
		}, "endpoint path"),
		Entry("no allow rule", execution.Endpoint{Host: "github.com", Port: 443, Protocol: "rest"}, "no allow rule"),
		Entry("a query glob", execution.Endpoint{
			Host: "github.com", Port: 443, Protocol: "rest",
			Allow: []execution.L7Rule{{Method: "GET", Path: "/x", Query: map[string]string{"service": "git-*"}}},
		}, "not an exact value"),
		Entry("a bad path pattern", execution.Endpoint{
			Host: "github.com", Port: 443, Protocol: "rest",
			Allow: []execution.L7Rule{{Method: "GET", Path: "x/../y"}},
		}, "egress routes"),
	)

	It("refuses an incomplete agent ref", func() {
		_, err := reconciler.EgressRoutes(gateway.AgentRef{Org: "acme"}, nil, true)
		Expect(err).To(HaveOccurred())
	})

	It("turns plugin installs on only for a harness with the option", func() {
		on := agentconfig.Config{}
		on.Harness.PluginInstalls = true
		Expect(reconciler.PluginInstallsOn(on, harness.OpenCode)).To(BeTrue())
		Expect(reconciler.PluginInstallsOn(agentconfig.Config{}, harness.OpenCode)).To(BeFalse())
		Expect(reconciler.PluginInstallsOn(on, "no-such-harness")).To(BeFalse())
	})
})

var _ = Describe("The sandbox rule of the gateway's egress listener (#1086 part C2)", func() {
	gw := reconciler.Endpoint{Host: "koinon-mcp-gateway.koinon.svc.cluster.local", Port: 8081}
	bins := []string{"/usr/bin/git", "/usr/lib/git-core/git-remote-http"}

	It("allows the grant's own requests under the egress prefix and nothing else on the listener", func() {
		cfg := agentconfig.Config{
			Grants:   []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings: []agentconfig.Binding{{Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000001", Targets: []string{"acme/widgets"}}},
		}
		read := mustRead(cfg)
		rule := reconciler.EgressRule(gw, read, bins)
		Expect(rule.Binaries).To(Equal(bins))
		Expect(rule.Endpoints).To(HaveLen(1))
		ep := rule.Endpoints[0]
		Expect(ep.Host).To(Equal(gw.Host))
		Expect(ep.Port).To(Equal(gw.Port))
		Expect(ep.Protocol).To(Equal("rest"), "enforced at L7")
		Expect(ep.Access).To(BeEmpty(), "no open access preset")

		var allow, deny int
		for _, e := range read.Endpoints {
			allow += len(e.Allow)
			deny += len(e.Deny)
		}
		Expect(ep.Allow).To(HaveLen(allow))
		Expect(ep.Deny).To(HaveLen(deny))
		for _, r := range append(append([]execution.L7Rule{}, ep.Allow...), ep.Deny...) {
			Expect(r.Path).To(Or(HavePrefix("/egress/github.com/"), HavePrefix("/egress/api.github.com/")), r.Path)
		}
		Expect(ep.Allow).To(ContainElement(HaveField("Path", "/egress/github.com/acme/widgets.git/git-upload-pack")))
		var wantAllow, wantDeny []execution.L7Rule
		for _, e := range read.Endpoints {
			prefix := "/egress/" + e.Host
			if e.Port != 0 && e.Port != 443 {
				prefix += fmt.Sprintf(":%d", e.Port)
			}
			for _, r := range e.Allow {
				wantAllow = append(wantAllow, execution.L7Rule{Method: r.Method, Path: prefix + r.Path, Query: r.Query})
			}
			for _, r := range e.Deny {
				wantDeny = append(wantDeny, execution.L7Rule{Method: r.Method, Path: prefix + r.Path, Query: r.Query})
			}
		}
		Expect(ep.Allow).To(Equal(wantAllow), "each forge rule, under the egress prefix")
		Expect(ep.Deny).To(Equal(wantDeny))
	})

	It("names a host on another port with its port, as koinon-agentd's rewrite does", func() {
		read := reconciler.GitHubRead{Endpoints: []execution.Endpoint{{
			Host: "ghes.example.test", Port: 8443, Protocol: "rest",
			Allow: []execution.L7Rule{{Method: "GET", Path: "/acme/widgets.git/info/refs", Query: map[string]string{"service": "git-upload-pack"}}},
		}}}
		ep := reconciler.EgressRule(gw, read, bins).Endpoints[0]
		Expect(ep.Allow).To(Equal([]execution.L7Rule{{Method: "GET", Path: "/egress/ghes.example.test:8443/acme/widgets.git/info/refs", Query: map[string]string{"service": "git-upload-pack"}}}))
	})
})
