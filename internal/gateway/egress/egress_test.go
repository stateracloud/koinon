// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/faketls"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// seen is what the fake upstream received.
type seen struct {
	Host, Method, URI string
	Header            http.Header
}

var _ = Describe("egress routes (behaviour: allow, deny, token)", func() {
	const (
		forgeHost = "git.example.test"
		regHost   = "registry.example.test"
		issuer    = "koinon-test"
	)
	var (
		upstream *httptest.Server
		port     int
		mu       sync.Mutex
		got      []seen
		minter   *actortoken.Minter
		otherMnt *actortoken.Minter
		ver      *actortoken.Verifier
		gw       *egress.Gateway
		gwSrv    *httptest.Server
		reg      *prometheus.Registry
		metrics  *egress.Metrics
		logs     *bytes.Buffer
		acme     = actortoken.Actor{Org: "acme", Agent: "builder", Run: "run-1"}
		answer   func(w http.ResponseWriter, r *http.Request)
	)

	routes := func() []route.Route {
		return []route.Route{
			{
				Name: "acme-forge", Class: route.ClassForge, Org: "acme", Host: forgeHost, Port: port,
				Methods: []string{"GET", "POST"}, PathPrefixes: []string{"/acme/app.git"}, QueryKeys: []string{"service"},
			},
			{
				Name: "acme-forge-cred", Class: route.ClassForge, Org: "acme", Host: forgeHost, Port: port,
				Methods: []string{"GET"}, PathPrefixes: []string{"/acme/private.git"}, Credential: "forge-acme",
			},
			{
				Name: "registry", Class: route.ClassRegistry, Host: regHost, Port: port,
				Methods: []string{"GET", "HEAD"}, PathPrefixes: []string{"/"},
			},
		}
	}

	token := func(m *actortoken.Minter, a actortoken.Actor) string {
		t, err := m.Mint(a, 0)
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	do := func(method, path, tok string, hdr http.Header) *http.Response {
		req, err := http.NewRequestWithContext(context.Background(), method, gwSrv.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		for k, v := range hdr {
			req.Header[k] = v
		}
		if tok != "" {
			req.Header.Set(route.TokenHeader, tok)
		}
		resp, err := gwSrv.Client().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}

	authority := func(host string) string { return host + ":" + strconv.Itoa(port) }

	count := func(class, reason string) float64 {
		return testutil.ToFloat64(metrics.Requests.WithLabelValues(class, reason))
	}

	upstreamCalls := func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), got...)
	}

	BeforeEach(func() {
		got = nil
		answer = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "upstream body") }
		set, err := faketls.Make("egress-test-ca", []string{forgeHost, regHost})
		Expect(err).NotTo(HaveOccurred())
		cert, err := tls.X509KeyPair(set.Cert, set.Key)
		Expect(err).NotTo(HaveOccurred())
		roots := x509.NewCertPool()
		Expect(roots.AppendCertsFromPEM(set.CA)).To(BeTrue())

		upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, seen{Host: r.Host, Method: r.Method, URI: r.RequestURI, Header: r.Header.Clone()})
			mu.Unlock()
			answer(w, r)
		}))
		upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		upstream.StartTLS()
		DeferCleanup(upstream.Close)
		port = upstream.Listener.Addr().(*net.TCPAddr).Port

		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		minter, err = actortoken.NewMinter(issuer, "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		otherMnt, err = actortoken.NewMinter(issuer, "k1", otherPriv)
		Expect(err).NotTo(HaveOccurred())
		ver, err = actortoken.NewVerifier(issuer, map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())

		reg = prometheus.NewRegistry()
		metrics = egress.NewMetrics(reg)
		logs = &bytes.Buffer{}
		addr := upstream.Listener.Addr().String()
		gw, err = egress.New(egress.Options{
			Identifier: ver,
			// Every route host resolves to the fake upstream.
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			RootCAs: roots,
			Metrics: metrics,
			Log:     slog.New(slog.NewJSONHandler(logs, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		table, err := route.Compile(routes())
		Expect(err).NotTo(HaveOccurred())
		gw.Apply(table)
		gwSrv = httptest.NewServer(gw.Handler())
		DeferCleanup(gwSrv.Close)
	})

	Context("allow", func() {
		It("forwards an allowed forge request to the exact host over HTTPS, with path and query", func() {
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs?service=git-upload-pack", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			b, _ := io.ReadAll(resp.Body)
			Expect(string(b)).To(Equal("upstream body"))
			calls := upstreamCalls()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].Host).To(Equal(authority(forgeHost)))
			Expect(calls[0].URI).To(Equal("/acme/app.git/info/refs?service=git-upload-pack"))
			Expect(count("forge", "allowed")).To(Equal(1.0))
		})

		It("serves a platform registry route to any org", func() {
			other := actortoken.Actor{Org: "globex", Agent: "a"}
			resp := do("HEAD", "/egress/"+authority(regHost)+"/left-pad/-/left-pad-1.3.0.tgz", token(minter, other), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(count("registry", "allowed")).To(Equal(1.0))
		})

		It("never sends the actor token or the guest's credentials upstream", func() {
			hdr := http.Header{
				"Authorization":       {"Bearer koinon_test_not-a-real-token"},
				"Cookie":              {"session=koinon_test_not-a-real-token"},
				"Proxy-Authorization": {"Basic koinon_test_not-a-real-token"},
				"Forwarded":           {"for=10.0.0.1"},
				"X-Forwarded-For":     {"10.0.0.1"},
				"User-Agent":          {"git/2.47"},
			}
			tok := token(minter, acme)
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs", tok, hdr)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			h := upstreamCalls()[0].Header
			for _, k := range []string{route.TokenHeader, "Authorization", "Cookie", "Proxy-Authorization", "Forwarded", "X-Forwarded-For"} {
				Expect(h.Values(k)).To(BeEmpty(), k)
			}
			Expect(h.Get("User-Agent")).To(Equal("git/2.47"))
			Expect(logs.String()).NotTo(ContainSubstring(tok))
			Expect(logs.String()).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
		})

		It("rewrites a redirect to the gateway form and does not follow it", func() {
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://CDN.example.test/blob/x?sig=1")
				w.WriteHeader(http.StatusFound)
			}
			client := *gwSrv.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			req, err := http.NewRequestWithContext(context.Background(), "GET", gwSrv.URL+"/egress/"+authority(regHost)+"/pkg", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set(route.TokenHeader, token(minter, acme))
			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.StatusCode).To(Equal(http.StatusFound))
			Expect(resp.Header.Get("Location")).To(Equal("/egress/cdn.example.test/blob/x?sig=1"))
			Expect(upstreamCalls()).To(HaveLen(1))
		})

		It("rewrites a host-relative redirect to the same upstream", func() {
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/pkg/next")
				w.WriteHeader(http.StatusMovedPermanently)
			}
			client := *gwSrv.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			req, err := http.NewRequestWithContext(context.Background(), "GET", gwSrv.URL+"/egress/"+authority(regHost)+"/pkg", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set(route.TokenHeader, token(minter, acme))
			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.Header.Get("Location")).To(Equal("/egress/" + authority(regHost) + "/pkg/next"))
		})

		It("drops a redirect to plain HTTP", func() {
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "http://registry.example.test/pkg")
				w.WriteHeader(http.StatusFound)
			}
			client := *gwSrv.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			req, err := http.NewRequestWithContext(context.Background(), "GET", gwSrv.URL+"/egress/"+authority(regHost)+"/pkg", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set(route.TokenHeader, token(minter, acme))
			resp, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.Header.Get("Location")).To(BeEmpty())
		})
	})

	Context("token", func() {
		DescribeTable("refuses with 401 and reaches no upstream",
			func(tok func() string, reason string) {
				resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs", tok(), nil)
				Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
				Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(reason))
				Expect(upstreamCalls()).To(BeEmpty())
				Expect(count("none", reason)).To(Equal(1.0))
			},
			Entry("no token", func() string { return "" }, "no_token"),
			Entry("a token of another key", func() string { return token(otherMnt, acme) }, "bad_token"),
			Entry("garbage", func() string { return "not.a.token" }, "bad_token"),
			Entry("an expired token", func() string {
				old := minter.WithClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
				return token(old, acme)
			}, "bad_token"),
		)

		It("refuses a token in the Authorization header: only the actor token header counts", func() {
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs", "",
				http.Header{"Authorization": {"Bearer " + token(minter, acme)}})
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
			Expect(upstreamCalls()).To(BeEmpty())
		})
	})

	Context("deny", func() {
		DescribeTable("refuses and reaches no upstream",
			func(method, path string, status int, reason string) {
				// The entries are built before the upstream port is known.
				path = strings.ReplaceAll(path, "PORT", strconv.Itoa(port))
				resp := do(method, path, token(minter, acme), nil)
				Expect(resp.StatusCode).To(Equal(status))
				Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal(reason))
				Expect(upstreamCalls()).To(BeEmpty())
			},
			Entry("a host with no route", "GET", "/egress/evil.example.test/x", http.StatusForbidden, "no_route"),
			Entry("the right host on another port", "GET", "/egress/"+forgeHost+":1/acme/app.git", http.StatusForbidden, "no_route"),
			Entry("a method the route does not allow", "DELETE", "/egress/git.example.test:PORT/acme/app.git", http.StatusForbidden, "method_not_allowed"),
			Entry("a path outside the route", "GET", "/egress/git.example.test:PORT/acme/other.git", http.StatusForbidden, "path_not_allowed"),
			Entry("a path that only shares a prefix", "GET", "/egress/git.example.test:PORT/acme/app.gitx", http.StatusForbidden, "path_not_allowed"),
			Entry("a query key the route does not allow", "GET", "/egress/git.example.test:PORT/acme/app.git?ref=x", http.StatusForbidden, "query_not_allowed"),
			Entry("a dot segment", "GET", "/egress/git.example.test:PORT/acme/app.git/../other.git", http.StatusBadRequest, "bad_request"),
			Entry("an encoded slash", "GET", "/egress/git.example.test:PORT/acme/app.git%2f..%2fother.git", http.StatusBadRequest, "bad_request"),
			Entry("an escape in the authority", "GET", "/egress/git%2eexample.test:PORT/acme/app.git", http.StatusBadRequest, "bad_request"),
			Entry("userinfo in the authority", "GET", "/egress/x@git.example.test:PORT/acme/app.git", http.StatusBadRequest, "bad_request"),
			Entry("no authority", "GET", "/egress//acme/app.git", http.StatusBadRequest, "bad_request"),
			Entry("a path outside the egress prefix", "GET", "/mcp", http.StatusBadRequest, "bad_request"),
		)

		It("refuses another org's route", func() {
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs",
				token(minter, actortoken.Actor{Org: "globex", Agent: "a"}), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
			// On the wire it looks like a host with no route, so a guest cannot learn the hosts of
			// other orgs. The metric and the log keep the true reason.
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("no_route"))
			b, _ := io.ReadAll(resp.Body)
			Expect(string(b)).NotTo(ContainSubstring("org_mismatch"))
			Expect(count("none", "org_mismatch")).To(Equal(1.0))
			Expect(logs.String()).To(ContainSubstring(`"result":"org_mismatch"`))
			Expect(upstreamCalls()).To(BeEmpty())
		})

		It("counts one result for a request whose upstream fails", func() {
			upstream.Close()
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(count("registry", "upstream_error")).To(Equal(1.0))
			Expect(count("registry", "allowed")).To(Equal(0.0))
			// The audit line names the actor of the failed request.
			Expect(logs.String()).To(ContainSubstring(`"result":"upstream_error"`))
			Expect(logs.String()).To(ContainSubstring(`"org":"acme"`))
			Expect(logs.String()).To(ContainSubstring(`"run":"run-1"`))
		})

		It("removes a refusal header that the upstream sends", func() {
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(egress.RefusalHeader, "no_route")
				_, _ = io.WriteString(w, "upstream body")
			}
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Values(egress.RefusalHeader)).To(BeEmpty())
		})

		It("writes no host to the log for a caller without a valid token", func() {
			resp := do("GET", "/egress/attacker-text.example.test/x", "", nil)
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
			Expect(logs.String()).To(ContainSubstring(`"result":"no_token"`))
			Expect(logs.String()).NotTo(ContainSubstring("attacker-text"))
		})

		It("closes a route whose credential is not in the State", func() {
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/private.git/info/refs", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("credential_unavailable"))
			Expect(upstreamCalls()).To(BeEmpty())
			Expect(count("forge", "credential_unavailable")).To(Equal(1.0))
		})

		It("refuses every request with no table", func() {
			gw.Apply(nil)
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("no_table"))
			Expect(upstreamCalls()).To(BeEmpty())
		})

		It("refuses a protocol upgrade", func() {
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme),
				http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}})
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(upstreamCalls()).To(BeEmpty())
		})

		It("refuses new requests while it drains", func() {
			gw.Drain()
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("draining"))
		})

		It("refuses every request while the install is held, before the token check, and logs no body or path (#1390)", func() {
			held, err := egress.New(egress.Options{
				Identifier: ver, Metrics: metrics, Log: slog.New(slog.NewJSONHandler(logs, nil)), Held: true,
				Dial: func(context.Context, string, string) (net.Conn, error) {
					defer GinkgoRecover()
					Fail("a held gateway dials no upstream")
					return nil, errors.New("held")
				},
			})
			Expect(err).NotTo(HaveOccurred())
			table, err := route.Compile(routes())
			Expect(err).NotTo(HaveOccurred())
			held.Apply(table)
			srv := httptest.NewServer(held.Handler())
			DeferCleanup(srv.Close)

			send := func(method, path, tok, body string) (int, string, string) {
				req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
				Expect(err).NotTo(HaveOccurred())
				if tok != "" {
					req.Header.Set(route.TokenHeader, tok)
				}
				resp, err := srv.Client().Do(req)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = resp.Body.Close() }()
				b, err := io.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())
				return resp.StatusCode, resp.Header.Get(egress.RefusalHeader), string(b)
			}
			// An allowed request with a valid token, a request with no token and one with no route: all held.
			for _, c := range []struct{ method, path, tok string }{
				{"POST", "/egress/" + authority(forgeHost) + "/acme/app.git/git-upload-pack?service=git-upload-pack", token(minter, acme)},
				{"GET", "/egress/" + authority(regHost) + "/held-path-marker", ""},
				{"GET", "/egress/evil.example.test/x", token(minter, acme)},
			} {
				code, reason, body := send(c.method, c.path, c.tok, "held-body-marker")
				Expect(code).To(Equal(http.StatusServiceUnavailable), c.path)
				Expect(reason).To(Equal("held"), c.path)
				Expect(body).To(ContainSubstring(restorehold.Message), c.path)
			}
			Expect(upstreamCalls()).To(BeEmpty())
			Expect(count("none", "held")).To(Equal(3.0))
			Expect(logs.String()).To(ContainSubstring(`"result":"held"`))
			Expect(logs.String()).NotTo(ContainSubstring("held-body-marker"))
			Expect(logs.String()).NotTo(ContainSubstring("held-path-marker"))
			Expect(logs.String()).NotTo(ContainSubstring(forgeHost))

			// The suite's gateway, which is not held, serves the same allowed request with the same table.
			resp := do("GET", "/egress/"+authority(regHost)+"/pkg", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(upstreamCalls()).To(HaveLen(1))
		})

		It("does not reach an internal address through an allowed host name", func() {
			policy := catalog.URLPolicy{
				Namespace: "koinon",
				Resolver: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
				},
			}
			strict, err := egress.New(egress.Options{
				Identifier: ver, Dial: policy.GatewayDialContext(&net.Dialer{Timeout: time.Second}), Metrics: egress.NewMetrics(nil),
			})
			Expect(err).NotTo(HaveOccurred())
			table, err := route.Compile(routes())
			Expect(err).NotTo(HaveOccurred())
			strict.Apply(table)
			srv := httptest.NewServer(strict.Handler())
			defer srv.Close()
			req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/egress/"+authority(regHost)+"/pkg", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set(route.TokenHeader, token(minter, acme))
			resp, err := srv.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("upstream_error"))
			Expect(upstreamCalls()).To(BeEmpty())
		})
	})

	Context("org routes from the State stream (#1086 part B)", func() {
		agentRoute := func(name string, deny bool) route.Route {
			r := route.Route{
				Name: "org/acme/agent/builder/" + name, Class: route.ClassForge, Org: "acme", Agent: "builder",
				Host: forgeHost, Port: port, Methods: []string{"GET", "POST"}, Paths: []string{"/acme/new.git/**"},
				AnyQuery: true, Credential: "org/acme/agent/builder/codehost",
			}
			if deny {
				r.Methods, r.Paths, r.AnyQuery, r.Credential, r.Deny = []string{"POST"}, []string{"/**/git-receive-pack"}, false, "", true
			}
			return r
		}
		platform := func() *route.Table {
			t, err := route.Compile([]route.Route{routes()[2]})
			Expect(err).NotTo(HaveOccurred())
			return t
		}

		It("merges the org routes with the platform routes", func() {
			st := egress.StateRoutes{Gateway: gw, Platform: platform()}
			Expect(st.ApplyEgress(context.Background(), gateway.EgressState{EgressRoutes: []route.Route{agentRoute("forge-0", false), agentRoute("forge-1", true)}})).To(Succeed())
			// A route whose credential is not in the State is enforced and refused.
			resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/new.git/info/refs?service=git-upload-pack", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("credential_unavailable"))
			// The deny route refuses a push before any allow.
			resp = do("POST", "/egress/"+authority(forgeHost)+"/acme/new.git/git-receive-pack", token(minter, acme), nil)
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("denied"))
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
			// Another agent of the org gets no route; the platform route still serves.
			other := actortoken.Actor{Org: "acme", Agent: "reviewer", Run: "run-2"}
			resp = do("GET", "/egress/"+authority(forgeHost)+"/acme/new.git/info/refs", token(minter, other), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("no_route"))
			resp = do("GET", "/egress/"+authority(regHost)+"/left-pad", token(minter, other), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(upstreamCalls()).To(HaveLen(1))
		})

		DescribeTable("serves the platform routes alone when the org routes are bad",
			func(mut func(r *route.Route)) {
				st := egress.StateRoutes{Gateway: gw, Platform: platform()}
				r := agentRoute("forge-0", false)
				r.Credential = ""
				mut(&r)
				Expect(st.ApplyEgress(context.Background(), gateway.EgressState{EgressRoutes: []route.Route{r}})).NotTo(Succeed())
				resp := do("GET", "/egress/"+authority(forgeHost)+"/acme/new.git/info/refs", token(minter, acme), nil)
				Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
				// The routes of the test's own table are gone too: the platform table alone serves.
				resp = do("GET", "/egress/"+authority(forgeHost)+"/acme/app.git/info/refs", token(minter, acme), nil)
				Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
				resp = do("GET", "/egress/"+authority(regHost)+"/left-pad", token(minter, acme), nil)
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
			},
			Entry("no org", func(r *route.Route) { r.Org, r.Agent = "", "" }),
			Entry("a platform-style name", func(r *route.Route) { r.Name = "registry" }),
			Entry("a bad path pattern", func(r *route.Route) { r.Paths = []string{"bad"} }),
			Entry("a wildcard host", func(r *route.Route) { r.Host = "*.example.test" }),
		)

		It("refuses every request with no platform file and bad org routes", func() {
			st := egress.StateRoutes{Gateway: gw}
			r := agentRoute("forge-0", false)
			r.Org = ""
			Expect(st.ApplyEgress(context.Background(), gateway.EgressState{EgressRoutes: []route.Route{r}})).NotTo(Succeed())
			resp := do("GET", "/egress/"+authority(regHost)+"/left-pad", token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("no_table"))
		})

		It("refuses two org routes with the same credential in two orgs", func() {
			a := agentRoute("forge-0", false)
			b := agentRoute("forge-0", false)
			b.Name, b.Org = "org/globex/agent/builder/forge-0", "globex"
			_, err := egress.Merge(platform(), []route.Route{a, b})
			Expect(err).To(MatchError(ContainSubstring("two orgs")))
		})
	})

	Context("credentials (#1086 part C)", func() {
		const value = "koinon_test_not-a-real-token"
		var (
			opener *fakeOpener
			st     egress.StateRoutes
			cred   route.Credential
		)
		credRoute := func() route.Route {
			return route.Route{
				Name: "org/acme/agent/builder/forge-0", Class: route.ClassForge, Org: "acme", Agent: "builder",
				Host: forgeHost, Port: port, Methods: []string{"GET"}, Paths: []string{"/acme/new.git/**"},
				AnyQuery: true, Credential: "org/acme/agent/builder/codehost",
			}
		}
		plainRoute := func() route.Route {
			r := credRoute()
			r.Name, r.Paths, r.Credential = "org/acme/agent/builder/forge-1", []string{"/acme/open.git/**"}, ""
			return r
		}
		path := func(p string) string { return "/egress/" + authority(forgeHost) + p }
		basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+value))
		var scrubbed []string
		var kinds []credkind.Kind

		BeforeEach(func() {
			opener = &fakeOpener{values: map[string]string{"/keys/forgev2-builder": value}}
			scrubbed = nil
			kinds = nil
			var err error
			gw, err = egress.New(egress.Options{
				Identifier: ver,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
				},
				RootCAs: rootsOf(upstream), Metrics: metrics, Log: slog.New(slog.NewJSONHandler(logs, nil)),
				Credentials: opener,
				Scrub: func(k credkind.Kind, _ string, v ...string) {
					kinds = append(kinds, k)
					scrubbed = append(scrubbed, v...)
				},
			})
			Expect(err).NotTo(HaveOccurred())
			gwSrv = httptest.NewServer(gw.Handler())
			DeferCleanup(gwSrv.Close)
			st = egress.StateRoutes{Gateway: gw}
			cred = route.Credential{
				Name: "org/acme/agent/builder/codehost", Org: "acme", Agent: "builder",
				File: "/keys/forgev2-builder", Scheme: route.SchemeBasic, User: "x-access-token",
			}
		})

		apply := func(c ...route.Credential) {
			Expect(st.ApplyEgress(context.Background(), gateway.EgressState{
				EgressRoutes: []route.Route{credRoute(), plainRoute()}, EgressCredentials: c,
			})).To(Succeed())
		}

		It("attaches the credential on the route that names it, and only there", func() {
			apply(cred)
			resp := do("GET", path("/acme/new.git/info/refs?service=git-upload-pack"), token(minter, acme),
				http.Header{"Authorization": {"Bearer koinon_test_guest-value"}})
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			resp = do("GET", path("/acme/open.git/info/refs"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			calls := upstreamCalls()
			Expect(calls).To(HaveLen(2))
			Expect(calls[0].Header.Values("Authorization")).To(Equal([]string{basic}))
			Expect(calls[1].Header.Values("Authorization")).To(BeEmpty())
			Expect(count("forge", "allowed")).To(Equal(2.0))
			Expect(scrubbed).To(ContainElements(value, basic))
			// The scrub reads the rule of the kind from the one list (#1221): a Basic route names its kind.
			Expect(kinds).To(HaveEach(credkind.EgressBasic))
			Expect(logs.String()).NotTo(ContainSubstring(value))
		})

		It("sends a bearer credential as a bearer token", func() {
			cred.Scheme, cred.User = route.SchemeBearer, ""
			apply(cred)
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(upstreamCalls()[0].Header.Get("Authorization")).To(Equal("Bearer " + value))
			Expect(scrubbed).To(ContainElements(value, "Bearer "+value))
			Expect(kinds).NotTo(BeEmpty())
			Expect(kinds).To(HaveEach(credkind.EgressBearer))
		})

		It("never serves the credential to another agent of the org", func() {
			apply(cred)
			other := actortoken.Actor{Org: "acme", Agent: "reviewer"}
			resp := do("GET", path("/acme/new.git/info/refs"), token(minter, other), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
			Expect(upstreamCalls()).To(BeEmpty())
			Expect(opener.calls).To(BeZero())
		})

		DescribeTable("refuses the route with credential_unavailable and reaches no upstream",
			func(setup func()) {
				setup()
				resp := do("GET", path("/acme/new.git/info/refs"), token(minter, acme), nil)
				Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
				Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("credential_unavailable"))
				Expect(upstreamCalls()).To(BeEmpty())
				Expect(count("forge", "credential_unavailable")).To(Equal(1.0))
				Expect(logs.String()).NotTo(ContainSubstring(value))
			},
			Entry("no credential in the State", func() { apply() }),
			Entry("a file that cannot be opened", func() {
				cred.File = "/keys/missing"
				apply(cred)
			}),
			Entry("a value too short to scrub", func() {
				opener.values[cred.File] = "short"
				apply(cred)
			}),
		)

		It("refuses a credential of another agent at apply, and serves the platform routes alone", func() {
			cred.Agent = "reviewer"
			err := st.ApplyEgress(context.Background(), gateway.EgressState{
				EgressRoutes: []route.Route{credRoute()}, EgressCredentials: []route.Credential{cred},
			})
			Expect(err).To(MatchError(ContainSubstring("serves its own agent only")))
			resp := do("GET", path("/acme/new.git/info/refs"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("no_table"))
		})

		It("masks the credential in the response headers and the body, also across reads", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Echo", "token="+value)
				w.Header().Set("X-Auth", basic)
				fl := w.(http.Flusher)
				_, _ = io.WriteString(w, "a "+value[:10])
				fl.Flush()
				_, _ = io.WriteString(w, value[10:]+" b "+strings.Repeat("x", 70000)+value+" end")
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			b, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).NotTo(ContainSubstring(value))
			Expect(string(b)).To(HavePrefix("a " + strings.Repeat("*", len(value)) + " b "))
			Expect(string(b)).To(HaveSuffix(strings.Repeat("*", len(value)) + " end"))
			Expect(resp.Header.Get("X-Echo")).To(Equal("token=" + strings.Repeat("*", len(value))))
			Expect(resp.Header.Get("X-Auth")).NotTo(ContainSubstring(basic[6:]))
		})

		It("asks for a body it can scan, and refuses an encoded one", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, r *http.Request) {
				// The transport asks for gzip itself; a guest value never reaches the upstream.
				Expect(r.Header.Get("Accept-Encoding")).To(Equal("gzip"))
				w.Header().Set("Content-Encoding", "br")
				_, _ = io.WriteString(w, "opaque")
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), http.Header{"Accept-Encoding": {"br"}})
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(count("forge", "upstream_error")).To(Equal(1.0))
			Expect(count("forge", "allowed")).To(Equal(0.0))
		})

		It("decodes a gzip body and masks it", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				zw := gzip.NewWriter(w)
				_, _ = io.WriteString(zw, "echo "+value)
				_ = zw.Close()
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			b, _ := io.ReadAll(resp.Body)
			Expect(string(b)).To(Equal("echo " + strings.Repeat("*", len(value))))
		})

		It("masks the credential in a trailer, whose value arrives after the body", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Trailer", "X-Echo-Trailer")
				_, _ = io.WriteString(w, "body")
				w.Header().Set("X-Echo-Trailer", "token="+value)
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			b, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(Equal("body"))
			Expect(resp.Trailer.Get("X-Echo-Trailer")).To(Equal("token=" + strings.Repeat("*", len(value))))
		})

		It("masks the credential in a trailer the upstream did not declare", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "body")
				w.(http.Flusher).Flush()
				w.Header().Set(http.TrailerPrefix+"X-Late", "token="+value)
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			_, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Trailer.Get("X-Late")).To(Equal("token=" + strings.Repeat("*", len(value))))
		})

		It("passes a response with no body, whatever its Content-Encoding says", func() {
			apply(cred)
			answer = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "br")
				w.WriteHeader(http.StatusNotModified)
			}
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.StatusCode).To(Equal(http.StatusNotModified))
			Expect(count("forge", "upstream_error")).To(Equal(0.0))
		})

		It("keeps the credential set and the table together: a new revision drops the old credential", func() {
			apply(cred)
			apply()
			resp := do("GET", path("/acme/new.git/x"), token(minter, acme), nil)
			Expect(resp.Header.Get(egress.RefusalHeader)).To(Equal("credential_unavailable"))
		})
	})

	It("needs a dialer", func() {
		_, err := egress.New(egress.Options{})
		Expect(err).To(HaveOccurred())
	})

	It("registers every class and reason series with fixed labels only", func() {
		mfs, err := reg.Gather()
		Expect(err).NotTo(HaveOccurred())
		Expect(mfs).To(HaveLen(1))
		for _, m := range mfs[0].GetMetric() {
			Expect(m.GetLabel()).To(HaveLen(2))
			for _, l := range m.GetLabel() {
				Expect(l.GetName()).To(BeElementOf("class", "reason"))
			}
		}
	})
})

// fakeOpener returns a fixed value per file.
type fakeOpener struct {
	mu     sync.Mutex
	values map[string]string
	calls  int
}

func (f *fakeOpener) Open(c route.Credential) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	v, ok := f.values[c.File]
	if !ok {
		return "", &egress.OpenError{Reason: "open", Err: errors.New("no such file")}
	}
	return v, nil
}

// rootsOf trusts the CA of the test upstream.
func rootsOf(s *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Certificate())
	return p
}
