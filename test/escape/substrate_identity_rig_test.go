// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// The identity stand-in of the actor identity specs (substrate_identity_test.go, issue #1386, ADR 0120
// amendment of 2026-11-26). It is the gateway's own egress code (egress.New, the Substrate verifier and
// identifier, route.Gate), served in this process, so a spec can see what reaches the gateway after
// atenet-egress: how many actor token values, whether a value is the placeholder, which actor the
// verified token names, and the forwarded-identity headers. The installed gateway cannot serve this
// role: it knows the actors of Koinon's State only, and the rig's sandboxes are not in it.
//
// The State of the stand-in is its ActorTable, which the specs fill with record. Its one route is a
// forge route of the recorded org and agent to forgeHost, and its dialer never connects. So a request
// whose identity the gateway accepts ends in 502 with Koinon-Egress-Refusal: upstream_error, and a
// refused one gets the refusal of route.Gate. No upstream is ever reached, and no credential exists.
//
// The listener serves plain HTTP and, on the same port, TLS with a certificate made for the run, so the
// spec of an https request can see whether that request carried a token.

const (
	// identityOrg and identityAgent are the org and agent that the stand-in's State gives each recorded
	// actor.
	identityOrg   = "koinon-escape-org-a"
	identityAgent = "koinon-escape-agent"
	// forgeHost is the code host of the stand-in's one route. It never resolves (RFC 6761).
	forgeHost = "forge.koinon-escape.invalid"
	// forgePrefix is the path prefix of the route. The segment after it is the probe id of a request.
	forgePrefix = "/koinon-escape/"
)

// acceptedReply reports whether a status and a refusal are the stand-in's answer to an accepted
// identity: the route allowed the request and the dial failed.
func acceptedReply(status int, refusal string) bool {
	return status == http.StatusBadGateway && refusal == string(egress.ReasonUpstream)
}

// forwardedHeader reports whether a request header carries a forwarded identity. The stand-in records such
// headers as they arrive.
func forwardedHeader(name string) bool {
	n := strings.ToLower(name)
	return n == "x-forwarded-client-cert" || n == "forwarded" || strings.HasPrefix(n, "x-forwarded-") ||
		strings.HasPrefix(n, "x-envoy-") || strings.HasPrefix(n, "x-client-cert")
}

// arrival is what the stand-in saw of one request. It holds no token: only the SHA-256 of each value.
type arrival struct {
	Probe       string
	Tokens      int      // the number of actor token header values
	SHA         []string // the SHA-256 (hex) of each value, in order
	Placeholder bool     // the first value is actortoken.Placeholder
	// Key is the actor that the first value names when it verifies; Verify is actortoken.Reason of the
	// error otherwise ("" when it verified, "none" when there is no value).
	Key       actortoken.SubstrateActor
	Verify    string
	Status    int
	Refusal   string
	TLS       bool
	Forwarded http.Header
}

// accepted reports whether the gateway accepted the identity of the request.
func (a arrival) accepted() bool { return acceptedReply(a.Status, a.Refusal) }

// identityGateway is the stand-in. Make it with startIdentityGateway.
type identityGateway struct {
	cfg      actortoken.SubstrateConfig
	verifier *actortoken.SubstrateVerifier
	actors   route.ActorTable
	handler  http.Handler

	mu       sync.Mutex
	state    map[actortoken.SubstrateActor]actortoken.Actor
	arrivals map[string][]arrival
	// tokens keeps, by probe id, the first value that verified, in memory only, so a spec can send it
	// again in this process (replay). It is never logged or written.
	tokens  map[string]string
	replays int
}

// startIdentityGateway starts the stand-in from the run's variables and stops it when the container
// ends. A missing variable fails the container: the specs that need the stand-in are the proof of
// ADR 0120's amendment, never a skip.
func startIdentityGateway(ctx context.Context) *identityGateway {
	GinkgoHelper()
	Expect(rig.identity).NotTo(BeNil(), "KOINON_ESCAPE_SUBSTRATE_IDENTITY_GATEWAY is not set: the actor identity specs need the identity stand-in as the code-host egress route (docs/dev/escape-substrate.md)")
	issuer := os.Getenv("KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER")
	Expect(issuer).NotTo(BeEmpty(), "KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_ISSUER is not set: the stand-in verifies the actor JWTs with the install's issuer")
	listen := os.Getenv("KOINON_ESCAPE_SUBSTRATE_IDENTITY_LISTEN")
	if listen == "" {
		listen = fmt.Sprintf(":%d", rig.identity.Port)
	}

	// The stand-in is the code-host egress route (substrate.EgressRoute), so atenet-egress mints its
	// tokens for the egress listener's audience (issue #1489), and the stand-in checks that one.
	cfg := actortoken.SubstrateConfig{
		Issuer: issuer, CAFile: os.Getenv("KOINON_ESCAPE_SUBSTRATE_ACTOR_JWT_CA_FILE"), Audience: actortoken.AudienceEgress,
	}
	v, err := actortoken.NewSubstrateVerifier(context.WithoutCancel(ctx), cfg)
	Expect(err).NotTo(HaveOccurred(), "the Substrate verifier of the stand-in")
	g := &identityGateway{
		cfg:      cfg,
		verifier: v,
		state:    map[actortoken.SubstrateActor]actortoken.Actor{},
		arrivals: map[string][]arrival{},
		tokens:   map[string]string{},
	}
	id, err := route.NewSubstrateIdentifier(v, &g.actors)
	Expect(err).NotTo(HaveOccurred())
	gw, err := egress.New(egress.Options{
		Identifier: id,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("koinon-escape: the stand-in connects to no upstream")
		},
		Log: slog.New(slog.DiscardHandler),
	})
	Expect(err).NotTo(HaveOccurred())
	table, err := route.Compile([]route.Route{{
		Name: "koinon-escape-forge", Class: route.ClassForge, Org: identityOrg, Agent: identityAgent,
		Host: forgeHost, Methods: []string{http.MethodGet}, PathPrefixes: []string{forgePrefix}, AnyQuery: true,
	}})
	Expect(err).NotTo(HaveOccurred())
	gw.Apply(table)
	g.handler = g.observe(gw.Handler())

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	Expect(err).NotTo(HaveOccurred(), "listen on %s for the stand-in", listen)
	srv := &http.Server{Handler: g.handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(newSniffListener(ln, selfSignedTLS())) }()
	DeferCleanup(func(ctx context.Context) error { return srv.Shutdown(ctx) })
	GinkgoWriter.Printf("identity stand-in on %s, reached by the guests as %s:%d\n", ln.Addr(), rig.identity.Host, rig.identity.Port)
	return g
}

// verifierFor returns a verifier of the install's issuer for the listener audience, with the clock now
// (nil: time.Now). The specs use it to check a token that the issuer really signed against another
// listener or another time.
func (g *identityGateway) verifierFor(ctx context.Context, audience string, now func() time.Time) *actortoken.SubstrateVerifier {
	GinkgoHelper()
	cfg := g.cfg
	cfg.Audience, cfg.Now = audience, now
	v, err := actortoken.NewSubstrateVerifier(context.WithoutCancel(ctx), cfg)
	Expect(err).NotTo(HaveOccurred())
	return v
}

// path is the request path of probe id on the stand-in's route.
func (g *identityGateway) path(probe string) string {
	return egress.PathPrefix + forgeHost + forgePrefix + probe + "/info/refs"
}

// record puts the actor k into the stand-in's State, as the reconciler records an actor.
func (g *identityGateway) record(k actortoken.SubstrateActor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state[k] = actortoken.Actor{Org: identityOrg, Agent: identityAgent}
	g.actors.Set(maps.Clone(g.state))
}

// forget removes the actor k from the State, as the reconciler does when the actor is deleted.
func (g *identityGateway) forget(k actortoken.SubstrateActor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.state, k)
	g.actors.Set(maps.Clone(g.state))
}

// seen returns what arrived for probe id.
func (g *identityGateway) seen(probe string) []arrival {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]arrival(nil), g.arrivals[probe]...)
}

// token returns the first verified value that arrived for probe id, for a replay in this process.
func (g *identityGateway) token(probe string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tokens[probe]
	return t, ok
}

// replay sends one request with the given actor token values to the stand-in in this process, and
// returns what the stand-in saw. It never leaves this process.
func (g *identityGateway) replay(values ...string) arrival {
	GinkgoHelper()
	g.mu.Lock()
	g.replays++
	probe := fmt.Sprintf("replay-%d", g.replays)
	g.mu.Unlock()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, g.path(probe), nil)
	for _, v := range values {
		r.Header.Add(route.TokenHeader, v)
	}
	g.handler.ServeHTTP(httptest.NewRecorder(), r)
	a := g.seen(probe)
	Expect(a).To(HaveLen(1))
	return a[0]
}

// observe records each request before and after the gateway's handler.
func (g *identityGateway) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe := ""
		if rest, ok := strings.CutPrefix(r.URL.Path, egress.PathPrefix+forgeHost+forgePrefix); ok {
			probe, _, _ = strings.Cut(rest, "/")
		}
		values := r.Header.Values(route.TokenHeader)
		a := arrival{Probe: probe, Tokens: len(values), TLS: r.TLS != nil, Forwarded: http.Header{}, Verify: "none"}
		for _, v := range values {
			a.SHA = append(a.SHA, sha256Hex(v))
		}
		verified := ""
		if len(values) > 0 {
			a.Placeholder = strings.TrimSpace(values[0]) == actortoken.Placeholder
			c, err := g.verifier.Verify(values[0])
			a.Verify = actortoken.Reason(err)
			if err == nil {
				a.Key, verified = c.Key(), values[0]
			}
		}
		for k, vs := range r.Header {
			if forwardedHeader(k) {
				a.Forwarded[k] = append([]string(nil), vs...)
			}
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.Status, a.Refusal = rec.status, rec.Header().Get(egress.RefusalHeader)
		g.mu.Lock()
		g.arrivals[probe] = append(g.arrivals[probe], a)
		if _, ok := g.tokens[probe]; !ok && verified != "" {
			g.tokens[probe] = verified
		}
		g.mu.Unlock()
	})
}

// sha256Hex is the SHA-256 of v in hex: how the stand-in names a token value without keeping it.
func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// statusRecorder keeps the status that the gateway's handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// selfSignedTLS is a TLS configuration with a certificate made for this run. No client trusts it, and
// no spec needs one to: the https spec asks only what reaches the stand-in.
func selfSignedTLS() *tls.Config {
	GinkgoHelper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "koinon-escape-identity"},
		DNSNames: []string{rig.identity.Host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// sniffListener serves TLS and plain HTTP on one port: a connection whose first byte is a TLS
// handshake record (0x16) gets TLS, any other plain HTTP. Each connection is sniffed in its own
// goroutine, so a slow client does not hold up the others.
type sniffListener struct {
	net.Listener
	tls   *tls.Config
	conns chan net.Conn
	errs  chan error
}

func newSniffListener(ln net.Listener, cfg *tls.Config) *sniffListener {
	l := &sniffListener{Listener: ln, tls: cfg, conns: make(chan net.Conn), errs: make(chan error, 1)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				l.errs <- err
				return
			}
			go l.sniff(c)
		}
	}()
	return l
}

func (l *sniffListener) sniff(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if first[0] == 0x16 {
		out = tls.Server(out, l.tls)
	}
	select {
	case l.conns <- out:
	case <-time.After(time.Minute):
		_ = c.Close()
	}
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		l.errs <- err
		return nil, err
	}
}

// peekedConn reads the bytes that the sniff peeked before the rest of the connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
