// SPDX-License-Identifier: Apache-2.0

package route_test

import (
	"crypto/ed25519"
	"crypto/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// The route model of #1086 part B: an agent scope, segment patterns, exact query values, any-query
// and deny routes, so the connector's network rules translate without loss (ADR 0120 2.2).
var _ = Describe("agent-scoped and pattern routes", func() {
	var (
		minter *actortoken.Minter
		ver    *actortoken.Verifier
		alice  = actortoken.Actor{Org: "acme", Agent: "alice", Run: "r1"}
		bob    = actortoken.Actor{Org: "acme", Agent: "bob", Run: "r2"}
		zed    = actortoken.Actor{Org: "zed", Agent: "alice", Run: "r3"}
	)
	BeforeEach(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		minter, err = actortoken.NewMinter("koinon-test", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		ver, err = actortoken.NewVerifier("koinon-test", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
	})

	gate := func(routes ...route.Route) *route.Gate {
		t, err := route.Compile(routes)
		Expect(err).NotTo(HaveOccurred())
		return route.NewGate(ver, t)
	}
	check := func(g *route.Gate, a actortoken.Actor, method, path, query string) route.Decision {
		tok, err := minter.Mint(a, 0)
		Expect(err).NotTo(HaveOccurred())
		return g.Check(route.Request{Scheme: "https", Token: tok, Authority: "git.example.com", Method: method, Path: path, RawQuery: query})
	}

	upload := route.Route{
		Name: "org/acme/agent/alice/forge-0", Class: route.ClassForge, Org: "acme", Agent: "alice", Host: "git.example.com",
		Methods: []string{"GET"}, Paths: []string{"/acme/app.git/info/refs"}, QueryValues: map[string]string{"service": "git-upload-pack"},
		AnyQuery: true, Credential: "org/acme/agent/alice/codehost",
	}
	api := route.Route{
		Name: "org/acme/agent/alice/forge-1", Class: route.ClassForge, Org: "acme", Agent: "alice", Host: "git.example.com",
		Methods: []string{"GET"}, Paths: []string{"/repos/acme/app/**"}, AnyQuery: true, Credential: "org/acme/agent/alice/codehost",
	}
	denyPush := route.Route{
		Name: "org/acme/agent/alice/forge-2", Class: route.ClassForge, Org: "acme", Agent: "alice", Host: "git.example.com",
		Methods: []string{"GET"}, Paths: []string{"/**/info/refs"}, QueryValues: map[string]string{"service": "git-receive-pack"}, Deny: true,
	}
	denyRecv := route.Route{
		Name: "org/acme/agent/alice/forge-3", Class: route.ClassForge, Org: "acme", Agent: "alice", Host: "git.example.com",
		Methods: []string{"POST"}, Paths: []string{"/**/git-receive-pack"}, Deny: true,
	}

	It("serves an agent's route to that agent only", func() {
		g := gate(upload, api)
		Expect(check(g, alice, "GET", "/acme/app.git/info/refs", "service=git-upload-pack").Allowed).To(BeTrue())
		d := check(g, bob, "GET", "/acme/app.git/info/refs", "service=git-upload-pack")
		Expect(d.Allowed).To(BeFalse())
		Expect(d.Reason).To(Equal(route.ReasonOrgMismatch))
		Expect(check(g, zed, "GET", "/repos/acme/app", "").Allowed).To(BeFalse())
	})

	It("serves an org route without an agent to every agent of the org", func() {
		org := api
		org.Name, org.Agent = "org/acme/shared", ""
		g := gate(org)
		Expect(check(g, alice, "GET", "/repos/acme/app/pulls", "").Allowed).To(BeTrue())
		Expect(check(g, bob, "GET", "/repos/acme/app/pulls", "").Allowed).To(BeTrue())
		Expect(check(g, zed, "GET", "/repos/acme/app/pulls", "").Allowed).To(BeFalse())
	})

	It("prefers the agent's route over the org's and the platform's", func() {
		org := api
		org.Name, org.Agent, org.Credential = "org/acme/shared", "", "org/acme/shared-cred"
		g := gate(api, org)
		d := check(g, alice, "GET", "/repos/acme/app/pulls", "")
		Expect(d.Allowed).To(BeTrue())
		Expect(d.Route.Name).To(Equal(api.Name))
		d = check(g, bob, "GET", "/repos/acme/app/pulls", "")
		Expect(d.Route.Name).To(Equal(org.Name))
	})

	DescribeTable("matches path patterns on decoded segments, case-sensitively",
		func(path string, want bool) {
			Expect(check(gate(api), alice, "GET", path, "").Allowed).To(Equal(want))
		},
		Entry("the base with **", "/repos/acme/app", true),
		Entry("a deeper path", "/repos/acme/app/pulls/1/files", true),
		Entry("an encoded letter", "/repos/acme/%61pp/pulls", true),
		Entry("another repository", "/repos/acme/apps/pulls", false),
		Entry("another case", "/repos/Acme/app/pulls", false),
		Entry("a prefix only", "/repos/acme", false),
		Entry("a dot segment", "/repos/acme/app/../other", false),
		Entry("an encoded slash", "/repos/acme%2fapp/pulls", false),
	)

	It("matches * as one non-empty segment", func() {
		r := api
		r.Paths = []string{"/repos/*/app"}
		g := gate(r)
		Expect(check(g, alice, "GET", "/repos/acme/app", "").Allowed).To(BeTrue())
		Expect(check(g, alice, "GET", "/repos/acme/x/app", "").Allowed).To(BeFalse())
		Expect(check(g, alice, "GET", "/repos//app", "").Allowed).To(BeFalse())
	})

	DescribeTable("requires each query value exactly",
		func(query string, want bool) {
			Expect(check(gate(upload), alice, "GET", "/acme/app.git/info/refs", query).Allowed).To(Equal(want))
		},
		Entry("the value", "service=git-upload-pack", true),
		Entry("the value and another key (any query)", "service=git-upload-pack&x=1", true),
		Entry("no value", "", false),
		Entry("another value", "service=git-receive-pack", false),
		Entry("the value twice and another", "service=git-upload-pack&service=git-receive-pack", false),
		Entry("another case", "service=GIT-UPLOAD-PACK", false),
	)

	It("refuses an unknown query key without any-query", func() {
		r := api
		r.AnyQuery = false
		r.QueryKeys = []string{"page"}
		g := gate(r)
		Expect(check(g, alice, "GET", "/repos/acme/app/pulls", "page=2").Allowed).To(BeTrue())
		Expect(check(g, alice, "GET", "/repos/acme/app/pulls", "page=2&x=1").Reason).To(Equal(route.ReasonQuery))
	})

	DescribeTable("refuses a push however it is spelled, before any allow",
		func(method, path, query string) {
			wide := api
			wide.Paths = []string{"/**"}
			wide.Methods = []string{"GET", "POST"}
			d := check(gate(wide, denyPush, denyRecv), alice, method, path, query)
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonDenied))
		},
		Entry("info/refs", "GET", "/acme/app.git/info/refs", "service=git-receive-pack"),
		Entry("info/refs in upper case", "GET", "/acme/app.git/INFO/REFS", "service=GIT-RECEIVE-PACK"),
		Entry("info/refs with a second value", "GET", "/acme/app.git/info/refs", "service=git-upload-pack&service=git-receive-pack"),
		Entry("receive-pack", "POST", "/acme/app.git/git-receive-pack", ""),
		Entry("receive-pack in upper case", "POST", "/acme/app.git/GIT-RECEIVE-PACK", ""),
		Entry("receive-pack with an encoded letter", "POST", "/acme/app.git/git%2Dreceive-pack", ""),
		Entry("the method in lower case", "post", "/acme/app.git/git-receive-pack", ""),
	)

	It("applies a deny route only in its scope", func() {
		wide := api
		wide.Name, wide.Agent, wide.Credential = "org/acme/shared", "", "org/acme/shared-cred"
		wide.Paths = []string{"/**"}
		wide.Methods = []string{"POST"}
		g := gate(wide, denyRecv)
		Expect(check(g, alice, "POST", "/acme/app.git/git-receive-pack", "").Reason).To(Equal(route.ReasonDenied))
		Expect(check(g, bob, "POST", "/acme/app.git/git-receive-pack", "").Allowed).To(BeTrue())
	})

	DescribeTable("refuses a route set with a bad route",
		func(mut func(r *route.Route)) {
			r := api
			r.Paths = []string{"/repos/acme/app/**"}
			mut(&r)
			_, err := route.Compile([]route.Route{r})
			Expect(err).To(HaveOccurred())
		},
		Entry("an agent without an org", func(r *route.Route) { r.Org, r.Credential = "", "" }),
		Entry("no path rule", func(r *route.Route) { r.Paths = nil }),
		Entry("an empty prefix list", func(r *route.Route) { r.Paths = nil; r.PathPrefixes = []string{} }),
		Entry("a relative pattern", func(r *route.Route) { r.Paths = []string{"repos/**"} }),
		Entry("a dot segment", func(r *route.Route) { r.Paths = []string{"/repos/../**"} }),
		Entry("a partial wildcard", func(r *route.Route) { r.Paths = []string{"/repos/a*"} }),
		Entry("a glob query value", func(r *route.Route) { r.QueryValues = map[string]string{"service": "git-*"} }),
		Entry("an empty query value", func(r *route.Route) { r.QueryValues = map[string]string{"service": ""} }),
		Entry("a bad query key", func(r *route.Route) { r.QueryValues = map[string]string{"a=b": "c"} }),
		Entry("a deny route with a credential", func(r *route.Route) { r.Deny = true }),
		Entry("a deny route with any query", func(r *route.Route) { r.Deny, r.Credential = true, "" }),
	)

	It("lists every reason", func() {
		Expect(route.Reasons()).To(ContainElements(route.ReasonDenied, route.ReasonOrgMismatch, route.ReasonNoRoute))
	})
})
