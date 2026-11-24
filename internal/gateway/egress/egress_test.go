// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
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
			Verifier: ver,
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

		It("closes a route that names a credential until the gateway can attach it", func() {
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

		It("does not reach an internal address through an allowed host name", func() {
			policy := catalog.URLPolicy{
				Namespace: "koinon",
				Resolver: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
				},
			}
			strict, err := egress.New(egress.Options{
				Verifier: ver, Dial: policy.GatewayDialContext(&net.Dialer{Timeout: time.Second}), Metrics: egress.NewMetrics(nil),
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
