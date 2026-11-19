// SPDX-License-Identifier: Apache-2.0

package route_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

var _ = Describe("forge and registry routes", func() {
	var (
		minter *actortoken.Minter
		ver    *actortoken.Verifier
		now    = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		acme   = actortoken.Actor{Org: "acme", Agent: "builder"}
		forge  = route.Route{
			Name: "acme-forge", Class: route.ClassForge, Org: "acme", Host: "git.example.com",
			Methods: []string{"GET", "POST"}, PathPrefixes: []string{"/acme/app.git", "/api/v1/repos/acme/app"},
			QueryKeys: []string{"service"}, Credential: "forge-acme",
		}
		npm = route.Route{
			Name: "npm", Class: route.ClassRegistry, Host: "registry.npmjs.org",
			Methods: []string{"GET", "HEAD"}, PathPrefixes: []string{"/"},
		}
		table *route.Table
		gate  *route.Gate
	)

	token := func(a actortoken.Actor) string {
		t, err := minter.Mint(a, 0)
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	BeforeEach(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		clock := func() time.Time { return now }
		minter, err = actortoken.NewMinter("koinon-test", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		minter = minter.WithClock(clock)
		ver, err = actortoken.NewVerifier("koinon-test", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
		ver = ver.WithClock(clock)
		table, err = route.Compile([]route.Route{forge, npm})
		Expect(err).NotTo(HaveOccurred())
		gate = route.NewGate(ver, table)
	})

	req := func(tok, authority, method, path, query string) route.Request {
		return route.Request{Scheme: "https", Token: tok, Authority: authority, Method: method, Path: path, RawQuery: query}
	}

	Describe("allow", func() {
		It("allows a forge call on a granted path with its credential name", func() {
			d := gate.Check(req(token(acme), "git.example.com", "POST", "/acme/app.git/git-upload-pack", "service=git-upload-pack"))
			Expect(d.Allowed).To(BeTrue(), string(d.Reason))
			Expect(d.Route.Name).To(Equal("acme-forge"))
			Expect(d.Route.Credential).To(Equal("forge-acme"))
			Expect(d.Actor).To(Equal(acme))
		})
		It("allows a registry read for any org, with no credential", func() {
			d := gate.Check(req(token(actortoken.Actor{Org: "zed", Agent: "a"}), "Registry.NPMJS.org:443", "HEAD", "/left-pad", ""))
			Expect(d.Allowed).To(BeTrue(), string(d.Reason))
			Expect(d.Route.Credential).To(BeEmpty())
		})
		It("reads the request from net/http and the token header", func() {
			r := httptest.NewRequest(http.MethodGet, "https://registry.npmjs.org/left-pad", nil)
			r.Header.Set(route.TokenHeader, token(acme))
			Expect(gate.Check(route.FromHTTP(r)).Allowed).To(BeTrue())
		})
	})

	Describe("deny", func() {
		DescribeTable("refuses with a fixed reason",
			func(authority, method, path, query string, want route.Reason) {
				d := gate.Check(req(token(acme), authority, method, path, query))
				Expect(d.Allowed).To(BeFalse())
				Expect(d.Reason).To(Equal(want))
				Expect(d.Route).To(Equal(route.Route{}))
			},
			Entry("unknown host", "evil.example.net", "GET", "/", "", route.ReasonNoRoute),
			Entry("a host that only ends like a granted host", "xregistry.npmjs.org", "GET", "/", "", route.ReasonNoRoute),
			Entry("another port", "git.example.com:8443", "GET", "/acme/app.git/info/refs", "", route.ReasonNoRoute),
			Entry("a write to a read-only registry", "registry.npmjs.org", "PUT", "/left-pad", "", route.ReasonMethod),
			Entry("a method the forge route does not list", "git.example.com", "DELETE", "/acme/app.git", "", route.ReasonMethod),
			Entry("another repository", "git.example.com", "GET", "/acme/other.git/info/refs", "", route.ReasonPath),
			Entry("a path that shares only a string prefix", "git.example.com", "GET", "/acme/app.gitx", "", route.ReasonPath),
			Entry("a query key not in the list", "git.example.com", "GET", "/acme/app.git/info/refs", "access_token=x", route.ReasonQuery),
			Entry("a query on a route with no keys", "registry.npmjs.org", "GET", "/left-pad", "a=b", route.ReasonQuery),
			Entry("a dot segment", "git.example.com", "GET", "/acme/app.git/../other.git", "", route.ReasonBadRequest),
			Entry("an encoded dot segment", "git.example.com", "GET", "/acme/app.git/%2e%2e/other.git", "", route.ReasonBadRequest),
			Entry("an encoded slash", "git.example.com", "GET", "/acme/app.git%2f..%2fother.git", "", route.ReasonBadRequest),
			Entry("a double slash", "git.example.com", "GET", "/acme//app.git", "", route.ReasonBadRequest),
			Entry("userinfo in the authority", "registry.npmjs.org@evil.example.net", "GET", "/", "", route.ReasonBadRequest),
			Entry("a dot segment with a parameter", "git.example.com", "GET", "/acme/app.git/..;/other.git", "", route.ReasonBadRequest),
			Entry("a port with trailing text", "git.example.com:443abc", "GET", "/acme/app.git", "", route.ReasonBadRequest),
			Entry("a signed port", "git.example.com:+443", "GET", "/acme/app.git", "", route.ReasonBadRequest),
			Entry("a non-ASCII host", "git.exampl\u00e9.com", "GET", "/acme/app.git", "", route.ReasonBadRequest),
			Entry("an empty authority", "", "GET", "/", "", route.ReasonBadRequest),
		)

		It("refuses a request that is not https", func() {
			r := req(token(acme), "registry.npmjs.org", "GET", "/left-pad", "")
			for _, sch := range []string{"", "http"} {
				r.Scheme = sch
				d := gate.Check(r)
				Expect(d.Allowed).To(BeFalse(), sch)
				Expect(d.Reason).To(Equal(route.ReasonBadRequest))
			}
		})
		It("sets the scheme from net/http: plain HTTP is refused", func() {
			r := httptest.NewRequest(http.MethodGet, "http://registry.npmjs.org/left-pad", nil)
			r.Header.Set(route.TokenHeader, token(acme))
			Expect(gate.Check(route.FromHTTP(r)).Allowed).To(BeFalse())
		})
		It("prefers the org's route over a platform route, whatever the order", func() {
			plat := route.Route{
				Name: "plat", Class: route.ClassForge, Host: "git.example.com", Methods: []string{"GET"},
				PathPrefixes: []string{"/acme"},
			}
			for _, rs := range [][]route.Route{{plat, forge}, {forge, plat}} {
				t, err := route.Compile(rs)
				Expect(err).NotTo(HaveOccurred())
				d := route.NewGate(ver, t).Check(req(token(acme), "git.example.com", "GET", "/acme/app.git/info/refs", ""))
				Expect(d.Route.Name).To(Equal("acme-forge"))
			}
		})

		It("refuses a method that is not in the exact case of the rule", func() {
			d := gate.Check(req(token(acme), "git.example.com", "get", "/acme/app.git/info/refs", ""))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonMethod))
		})
		It("ignores an absolute-form https URL on a plain connection", func() {
			r := httptest.NewRequest(http.MethodGet, "/left-pad", nil)
			r.Host = "registry.npmjs.org"
			r.URL.Scheme = "https"
			r.Header.Set(route.TokenHeader, token(acme))
			Expect(gate.Check(route.FromHTTP(r)).Allowed).To(BeFalse())
		})
		It("picks the longest matching prefix, whatever the order", func() {
			wide := route.Route{
				Name: "wide", Class: route.ClassForge, Org: "acme", Host: "git2.example.com",
				Methods: []string{"GET"}, PathPrefixes: []string{"/"}, Credential: "ro-acme",
			}
			narrow := wide
			narrow.Name, narrow.PathPrefixes, narrow.Credential = "narrow", []string{"/repos/acme"}, "rw-acme"
			for _, rs := range [][]route.Route{{wide, narrow}, {narrow, wide}} {
				t, err := route.Compile(rs)
				Expect(err).NotTo(HaveOccurred())
				d := route.NewGate(ver, t).Check(req(token(acme), "git2.example.com", "GET", "/repos/acme/x", ""))
				Expect(d.Route.Credential).To(Equal("rw-acme"))
			}
		})
		It("does not let a caller widen a compiled route through the returned values", func() {
			d := gate.Check(req(token(acme), "git.example.com", "GET", "/acme/app.git/info/refs", ""))
			d.Route.Methods[0] = "DELETE"
			table.Routes()[0].PathPrefixes[0] = "/"
			Expect(gate.Check(req(token(acme), "git.example.com", "DELETE", "/acme/app.git", "")).Allowed).To(BeFalse())
			Expect(gate.Check(req(token(acme), "git.example.com", "GET", "/other", "")).Allowed).To(BeFalse())
		})

		It("refuses another org's grant", func() {
			d := gate.Check(req(token(actortoken.Actor{Org: "zed", Agent: "a"}), "git.example.com", "GET", "/acme/app.git/info/refs", ""))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonOrgMismatch))
		})
	})

	Describe("token checks", func() {
		It("refuses a request with no token before it looks at routes", func() {
			Expect(gate.Check(req("", "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonNoToken))
		})
		It("refuses a garbage token", func() {
			Expect(gate.Check(req("not-a-jwt", "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonBadToken))
		})
		It("refuses an expired token", func() {
			late := route.NewGate(ver.WithClock(func() time.Time { return now.Add(2 * time.Hour) }), table)
			Expect(late.Check(req(token(acme), "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonBadToken))
		})
		It("refuses a token signed by another key", func() {
			_, other, err := ed25519.GenerateKey(rand.Reader)
			Expect(err).NotTo(HaveOccurred())
			m, err := actortoken.NewMinter("koinon-test", "k1", other)
			Expect(err).NotTo(HaveOccurred())
			tok, err := m.WithClock(func() time.Time { return now }).Mint(acme, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(gate.Check(req(tok, "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonBadToken))
		})
		It("refuses everything when there is no table or no verifier", func() {
			tok := token(acme)
			Expect(route.NewGate(ver, nil).Check(req(tok, "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonNoTable))
			Expect(route.NewGate(nil, table).Check(req(tok, "registry.npmjs.org", "GET", "/", "")).Reason).To(Equal(route.ReasonNoTable))
			var g *route.Gate
			Expect(g.Check(req(tok, "registry.npmjs.org", "GET", "/", "")).Allowed).To(BeFalse())
		})
	})

	Describe("loading the route table", func() {
		bad := func(mut func(*route.Route)) error {
			r := npm
			mut(&r)
			_, err := route.Compile([]route.Route{r})
			return err
		}
		DescribeTable("refuses the whole set when one route is wrong",
			func(mut func(*route.Route)) { Expect(bad(mut)).To(HaveOccurred()) },
			Entry("wildcard host", func(r *route.Route) { r.Host = "*.npmjs.org" }),
			Entry("host with a path", func(r *route.Route) { r.Host = "registry.npmjs.org/x" }),
			Entry("upper-case host", func(r *route.Route) { r.Host = "Registry.npmjs.org" }),
			Entry("IP literal", func(r *route.Route) { r.Host = "10.0.0.1" }),
			Entry("single label host", func(r *route.Route) { r.Host = "localhost" }),
			Entry("no method rule", func(r *route.Route) { r.Methods = nil }),
			Entry("wildcard method", func(r *route.Route) { r.Methods = []string{"*"} }),
			Entry("lower-case method", func(r *route.Route) { r.Methods = []string{"get"} }),
			Entry("no path rule", func(r *route.Route) { r.PathPrefixes = nil }),
			Entry("wildcard path", func(r *route.Route) { r.PathPrefixes = []string{"/v2/*"} }),
			Entry("relative path", func(r *route.Route) { r.PathPrefixes = []string{"v2"} }),
			Entry("dot segment path", func(r *route.Route) { r.PathPrefixes = []string{"/a/../b"} }),
			Entry("unknown class", func(r *route.Route) { r.Class = "model" }),
			Entry("no name", func(r *route.Route) { r.Name = "" }),
			Entry("port out of range", func(r *route.Route) { r.Port = 70000 }),
		)

		It("refuses one credential that serves two route classes", func() {
			a, b := forge, npm
			b.Credential, b.Org = a.Credential, a.Org
			_, err := route.Compile([]route.Route{a, b})
			Expect(err).To(MatchError(ContainSubstring("one route class")))
		})
		It("refuses one credential used by two orgs", func() {
			a, b := forge, forge
			b.Name, b.Org = "evil-forge", "evil"
			_, err := route.Compile([]route.Route{a, b})
			Expect(err).To(MatchError(ContainSubstring("one org")))
		})
		It("refuses a host with a trailing dot and a path prefix with a parameter", func() {
			Expect(bad(func(r *route.Route) { r.Host = "registry.npmjs.org." })).To(HaveOccurred())
			Expect(bad(func(r *route.Route) { r.PathPrefixes = []string{"/a;b"} })).To(HaveOccurred())
		})
		It("allows one credential on two routes of one class", func() {
			a, b := forge, forge
			b.Name, b.Host = "acme-forge-2", "git2.example.com"
			_, err := route.Compile([]route.Route{a, b})
			Expect(err).NotTo(HaveOccurred())
		})
		It("refuses a credential on a platform route", func() {
			Expect(bad(func(r *route.Route) { r.Credential = "shared" })).To(MatchError(ContainSubstring("platform route")))
		})
		It("refuses two routes with one name", func() {
			_, err := route.Compile([]route.Route{npm, npm})
			Expect(err).To(HaveOccurred())
		})
		It("holds a copy, so a later change of the input does not open a route", func() {
			in := []route.Route{npm}
			in[0].Methods = []string{"GET", "HEAD"}
			t, err := route.Compile(in)
			Expect(err).NotTo(HaveOccurred())
			in[0].Host = "evil.example.net"
			in[0].Methods[0] = "DELETE"
			Expect(t.Routes()[0].Host).To(Equal("registry.npmjs.org"))
			Expect(t.Routes()[0].Methods).To(Equal([]string{"GET", "HEAD"}))
		})
	})
})
