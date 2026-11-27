// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/harness"
)

const routerName = "atenet-router.ate-system.svc"

// pki is a CA that issues the certificates of one test: the router's serving certificate and the
// clients' SPIFFE certificates.
type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	n    int64
}

func newPKI(cn string) *pki {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	return &pki{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), n: 1}
}

// issue returns the key and the certificate chain of a leaf, as the PEM of a credential bundle.
func (p *pki) issue(tmpl *x509.Certificate) (bundle []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	p.n++
	tmpl.SerialNumber = big.NewInt(p.n)
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &key.PublicKey, p.key)
	Expect(err).NotTo(HaveOccurred())
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

func (p *pki) server(name string) []byte {
	return p.issue(&x509.Certificate{DNSNames: []string{name}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
}

func (p *pki) client(id string) []byte {
	u, err := url.Parse(id)
	Expect(err).NotTo(HaveOccurred())
	return p.issue(&x509.Certificate{URIs: []*url.URL{u}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
}

// fakeRouter is a gRPC server on TLS that, like the atenet router, requires a client certificate and lets
// only the SPIFFE IDs of its allow-list call.
type fakeRouter struct {
	lis     *bufconn.Listener
	mu      sync.Mutex
	allowed map[string]bool
	seen    []string
}

func startRouter(serving []byte, clientCA *pki, allowed ...string) *fakeRouter {
	r := &fakeRouter{lis: bufconn.Listen(1 << 16), allowed: map[string]bool{}}
	for _, id := range allowed {
		r.allowed[id] = true
	}
	cert, err := tlsKeyPair(serving)
	Expect(err).NotTo(HaveOccurred())
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(serverTLS(cert, pool))),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			id := ""
			if p, ok := peer.FromContext(ctx); ok {
				if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 && len(ti.State.PeerCertificates[0].URIs) > 0 {
					id = ti.State.PeerCertificates[0].URIs[0].String()
				}
			}
			r.mu.Lock()
			r.seen = append(r.seen, id)
			ok := r.allowed[id]
			r.mu.Unlock()
			if !ok {
				return nil, grpcPermissionDenied(id)
			}
			return h(ctx, req)
		}),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(r.lis) }()
	DeferCleanup(srv.Stop)
	return r
}

func (r *fakeRouter) lastSeen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return ""
	}
	return r.seen[len(r.seen)-1]
}

// call opens a new connection with creds and makes one call.
func (r *fakeRouter) call(ctx context.Context, creds credentials.TransportCredentials) error {
	conn, err := grpc.NewClient("passthrough:///router",
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return r.lis.DialContext(ctx) }))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(cctx, &healthpb.HealthCheckRequest{})
	return err
}

func tlsKeyPair(bundle []byte) (tls.Certificate, error) { return tls.X509KeyPair(bundle, bundle) }

func serverTLS(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs, MinVersion: tls.VersionTLS13}
}

func grpcPermissionDenied(id string) error {
	return status.Errorf(codes.PermissionDenied, "%q is not on the allow-list", id)
}

// startFakeControl starts a fake Substrate and returns it with the connection to its control API. The
// router of the fake is not used.
func startFakeControl() (*fakeate.Cluster, *grpc.ClientConn) {
	cl := fakeate.New()
	control, router, err := cl.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(cl.Stop)
	DeferCleanup(control.Close)
	DeferCleanup(router.Close)
	return cl, control
}

func testConfig(guestReady time.Duration) substrate.Config {
	return substrate.Config{
		PollInterval: 5 * time.Millisecond, GuestReadyTimeout: guestReady, DrainTimeout: 200 * time.Millisecond,
		DefaultTier: harness.TierDev, GuestSigner: fakeate.Signer(), GatewayRoutes: gatewayRoutes,
	}
}

func write(dir, name string, b []byte) string {
	p := filepath.Join(dir, name)
	Expect(os.WriteFile(p, b, 0o600)).To(Succeed())
	return p
}

var _ = Describe("the router connection", func() {
	const agentd = "spiffe://cluster.local/ns/koinon/sa/koinon-agentd"
	var (
		dir     string
		servers *pki
		clients *pki
	)
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		servers, clients = newPKI("servicedns"), newPKI("podidentity")
	})
	creds := func(ep substrate.Endpoint) credentials.TransportCredentials {
		c, err := substrate.RouterCredentials(ep)
		Expect(err).NotTo(HaveOccurred())
		return c
	}

	It("calls the router with the agentd pod certificate, over TLS with the router's CA", func(ctx SpecContext) {
		r := startRouter(servers.server(routerName), clients, agentd)
		ep := substrate.Endpoint{
			RouterAddr: routerName + ":443", RouterCAFile: write(dir, "ca.pem", servers.pem),
			RouterCertFile: write(dir, "bundle.pem", clients.client(agentd)),
		}
		Expect(r.call(ctx, creds(ep))).To(Succeed())
		Expect(r.lastSeen()).To(Equal(agentd))
	})

	It("checks the host of the router address when no server name is set, and the given name otherwise", func(ctx SpecContext) {
		r := startRouter(servers.server(routerName), clients, agentd)
		ep := substrate.Endpoint{
			RouterAddr: "atenet-router.ate-system.svc.cluster.local:443", RouterCAFile: write(dir, "ca.pem", servers.pem),
			RouterCertFile: write(dir, "bundle.pem", clients.client(agentd)),
		}
		Expect(r.call(ctx, creds(ep))).To(MatchError(ContainSubstring("x509")), "the certificate names another host")
		ep.RouterServerName = routerName
		Expect(r.call(ctx, creds(ep))).To(Succeed())
	})

	It("refuses a router whose certificate another CA signed", func(ctx SpecContext) {
		r := startRouter(servers.server(routerName), clients, agentd)
		ep := substrate.Endpoint{
			RouterAddr: routerName + ":443", RouterCAFile: write(dir, "ca.pem", newPKI("rogue").pem),
			RouterCertFile: write(dir, "bundle.pem", clients.client(agentd)),
		}
		Expect(r.call(ctx, creds(ep))).To(MatchError(ContainSubstring("x509")))
		Expect(r.seen).To(BeEmpty(), "no call reached the router")
	})

	It("falls back to the control API's CA file when no router CA is set", func(ctx SpecContext) {
		r := startRouter(servers.server(routerName), clients, agentd)
		ep := substrate.Endpoint{
			RouterAddr: routerName + ":443", CAFile: write(dir, "ca.pem", servers.pem),
			RouterCertFile: write(dir, "bundle.pem", clients.client(agentd)),
		}
		Expect(r.call(ctx, creds(ep))).To(Succeed())
	})

	It("is refused by the router without a client certificate, and with an identity that is not on the allow-list", func(ctx SpecContext) {
		r := startRouter(servers.server(routerName), clients, agentd)
		ca := write(dir, "ca.pem", servers.pem)
		Expect(r.call(ctx, creds(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCAFile: ca}))).NotTo(Succeed())
		other := write(dir, "other.pem", clients.client("spiffe://cluster.local/ns/koinon/sa/koinon-reconciler"))
		err := r.call(ctx, creds(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCAFile: ca, RouterCertFile: other}))
		Expect(err).To(MatchError(ContainSubstring("not on the allow-list")))
	})

	It("loads the client certificate again after a rotation, and the CA after a CA rotation", func(ctx SpecContext) {
		first := clients.client(agentd)
		certFile := write(dir, "bundle.pem", first)
		caFile := write(dir, "ca.pem", servers.pem)
		r := startRouter(servers.server(routerName), clients, agentd)
		c := creds(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCAFile: caFile, RouterCertFile: certFile})
		Expect(r.call(ctx, c)).To(Succeed())

		// The kubelet replaces the file with a new certificate for the same identity.
		rotated := clients.client(agentd)
		Expect(os.Remove(certFile)).To(Succeed())
		write(dir, "bundle.pem", rotated)
		Expect(r.call(ctx, c)).To(Succeed())

		// A new serving certificate from a new CA: the client trusts it once the CA file has changed, with no restart.
		newServers := newPKI("servicedns-2")
		r2 := startRouter(newServers.server(routerName), clients, agentd)
		Expect(r2.call(ctx, c)).To(MatchError(ContainSubstring("x509")))
		Expect(os.Remove(caFile)).To(Succeed())
		write(dir, "ca.pem", append(append([]byte{}, servers.pem...), newServers.pem...))
		Expect(r2.call(ctx, c)).To(Succeed())
	})

	It("fails at dial time on a missing or malformed certificate or CA file", func() {
		_, err := substrate.RouterCredentials(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCertFile: filepath.Join(dir, "missing.pem")})
		Expect(err).To(MatchError(ContainSubstring("router client certificate")))
		_, err = substrate.RouterCredentials(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCertFile: write(dir, "junk.pem", []byte("junk"))})
		Expect(err).To(MatchError(ContainSubstring("router client certificate")))
		_, err = substrate.RouterCredentials(substrate.Endpoint{RouterAddr: routerName + ":443", RouterCAFile: write(dir, "junkca.pem", []byte("junk"))})
		Expect(err).To(MatchError(ContainSubstring("no certificates")))
	})
})

var _ = Describe("a router that cannot be reached", func() {
	const routerName = "atenet-router.ate-system.svc"
	const agentd = "spiffe://cluster.local/ns/koinon/sa/koinon-agentd"

	// driverOn returns a driver on a router connection of Dial's kind, with an extra dial option.
	driverOn := func(ctx context.Context, ep substrate.Endpoint, guestReady time.Duration, opts ...grpc.DialOption) *substrate.Driver {
		_, control := startFakeControl()
		router, refusal, err := substrate.DialRouter(ep, opts...)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(router.Close)
		d := substrate.NewWithRouterRefusal(control, router, refusal, testConfig(guestReady))
		_, err = d.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		ready(ctx, d, "sb")
		return d
	}

	It("keeps ErrUnavailable and retries for a router that refuses the connection, also in a new process", func(ctx SpecContext) {
		servers := newPKI("servicedns")
		dir := GinkgoT().TempDir()
		// The router address has no listener: the connection never opens (an outage, not an install error).
		d := driverOn(ctx, substrate.Endpoint{
			RouterAddr: "127.0.0.1:1", RouterServerName: routerName, RouterCAFile: write(dir, "ca.pem", servers.pem),
		}, 600*time.Millisecond)
		started := time.Now()
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(execution.ErrUnreachable))
		Expect(err).To(MatchError(execution.ErrUnavailable))
		Expect(time.Since(started)).To(BeNumerically(">=", 500*time.Millisecond), "the driver waits for the router")
	})

	It("fails an Exec at once with ErrUnreachable and the TLS error when the router refuses the client certificate", func(ctx SpecContext) {
		servers, clients, other := newPKI("servicedns"), newPKI("podidentity"), newPKI("another")
		dir := GinkgoT().TempDir()
		r := startRouter(servers.server(routerName), clients, agentd)
		d := driverOn(ctx, substrate.Endpoint{
			RouterAddr: "passthrough:///router", RouterServerName: routerName,
			RouterCAFile: write(dir, "ca.pem", servers.pem), RouterCertFile: write(dir, "bundle.pem", other.client(agentd)),
		}, 30*time.Second, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return r.lis.DialContext(ctx) }))
		started := time.Now()
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrUnreachable))
		Expect(err).NotTo(MatchError(execution.ErrUnavailable))
		Expect(err).To(MatchError(ContainSubstring("tls:")))
		Expect(time.Since(started)).To(BeNumerically("<", 10*time.Second), "GuestReadyTimeout is 30 s")
		Expect(r.lastSeen()).To(BeEmpty(), "no call reached the router")
	})

	It("fails an Exec at once with ErrUnreachable when the driver does not trust the router certificate", func(ctx SpecContext) {
		servers, clients := newPKI("servicedns"), newPKI("podidentity")
		dir := GinkgoT().TempDir()
		r := startRouter(servers.server(routerName), clients, agentd)
		d := driverOn(ctx, substrate.Endpoint{
			RouterAddr: "passthrough:///router", RouterServerName: routerName,
			RouterCAFile: write(dir, "ca.pem", newPKI("rogue").pem), RouterCertFile: write(dir, "bundle.pem", clients.client(agentd)),
		}, 30*time.Second, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return r.lis.DialContext(ctx) }))
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrUnreachable))
		Expect(err).To(MatchError(ContainSubstring("x509")))
	})

	It("keeps ErrUnavailable for a router that served a call and then went away", func(ctx SpecContext) {
		d, cl := newDriver(testConfig(500 * time.Millisecond))
		ready(ctx, d, "sb")
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).NotTo(HaveOccurred())
		cl.StopRouter()
		_, err = d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(execution.ErrUnreachable))
	})
})
