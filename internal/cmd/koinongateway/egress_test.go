// SPDX-License-Identifier: Apache-2.0

package koinongateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

const actorIssuer = "https://koinon-api.koinon.svc"

// writeActorKey writes a fresh Ed25519 public key as dir/<kid>.pub and returns the minter of its pair.
func writeActorKey(dir, kid string) *actortoken.Minter {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	b, err := actortoken.MarshalPublicKey(pub)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, kid+actortoken.PublicKeyExt), b, 0o600)).To(Succeed())
	m, err := actortoken.NewMinter(actorIssuer, kid, priv)
	Expect(err).NotTo(HaveOccurred())
	return m
}

var _ = Describe("koinon-gateway's egress listener", func() {
	var tokenFile, keysDir string
	BeforeEach(func() {
		tokenFile = filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(fakeStreamToken+"\n"), 0o600)).To(Succeed())
		keysDir = GinkgoT().TempDir()
	})

	base := func(extra ...string) []string {
		return append([]string{"--state-url", stateURL, "--state-token-file", tokenFile}, extra...)
	}

	It("is off by default and needs the actor token issuer, its keys and the namespace when it is on", func() {
		Expect(cli.Check(NewRootCommand(), base(), nil)).To(Succeed())
		full := []string{"--egress-addr", ":8090", "--actor-token-issuer", actorIssuer, "--actor-token-keys-dir", keysDir, "--namespace", "koinon"}
		Expect(cli.Check(NewRootCommand(), base(full...), nil)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), base(full[:6]...), map[string]string{"POD_NAMESPACE": "koinon"})).To(Succeed())

		for i, want := range map[int]string{2: "actor-token-issuer", 4: "actor-token-keys-dir", 6: "namespace"} {
			args := append(append([]string{}, full[:i]...), full[i+2:]...)
			err := cli.Check(NewRootCommand(), base(args...), nil)
			Expect(err).To(MatchError(ContainSubstring(want)), want)
		}
	})

	It("selects the actor token verifier: ed25519 by default, substrate with its issuer and CA file (issue #1386)", func() {
		eg := []string{"--egress-addr", ":8090", "--namespace", "koinon"}
		ed := []string{"--actor-token-issuer", actorIssuer, "--actor-token-keys-dir", keysDir}
		sub := []string{"--actor-token-verifier", "substrate", "--actor-jwt-issuer", "https://idp.ate-system.svc", "--actor-jwt-ca-file", "/etc/koinon/substrate-idp-ca/ca.crt"}
		check := func(args ...[]string) error {
			all := []string{}
			for _, a := range args {
				all = append(all, a...)
			}
			return cli.Check(NewRootCommand(), base(all...), nil)
		}
		Expect(check(eg, ed)).To(Succeed())
		Expect(check(eg, ed, []string{"--actor-token-verifier", "ed25519"})).To(Succeed())
		Expect(check(eg, sub)).To(Succeed(), "substrate needs no Ed25519 key")

		Expect(check(eg, sub[:4])).To(MatchError(ContainSubstring("--actor-jwt-ca-file")))
		Expect(check(eg, sub[:2], sub[4:])).To(MatchError(ContainSubstring("--actor-jwt-issuer")))
		Expect(check(eg, sub, ed)).To(MatchError(ContainSubstring("are for --actor-token-verifier=ed25519")))
		Expect(check(eg, ed, sub[2:])).To(MatchError(ContainSubstring("need --actor-token-verifier=substrate")))
		Expect(check(eg, ed, []string{"--actor-token-verifier", "none"})).To(MatchError(ContainSubstring(`not "none"`)))
		Expect(check(eg, ed, []string{"--actor-token-verifier", ""})).To(MatchError(ContainSubstring("must be ed25519 or substrate")))
	})

	It("starts the substrate verifier without contacting the issuer, refuses on a bad config, and fails closed (issue #1386)", func(specCtx SpecContext) {
		// A reserved host: no request in this spec is allowed, so nothing dials it.
		routes := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(routes, []byte(`{"routes":[{"name":"reg","class":"registry","host":"registry.example.test","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())
		cfg := config{
			mcpPath: true, stateURL: "http://" + freeAddr(), stateTokenFile: tokenFile, listenAddr: freeAddr(), healthAddr: freeAddr(), egressAddr: freeAddr(),
			drain: 5 * time.Second, podName: "koinon-gateway-0", namespace: "koinon", egressRoutesFile: routes,
			// No server listens at the issuer: the start must not need it.
			actorTokenVerifier: verifierSubstrate, actorJWTIssuer: "https://" + freeAddr(),
			actorJWTCAFile: filepath.Join(GinkgoT().TempDir(), "missing.pem"),
		}
		Expect(run(specCtx, cfg)).To(MatchError(ContainSubstring("Substrate CA file")), "a missing CA file stops the start")
		notPEM := filepath.Join(GinkgoT().TempDir(), "ca.pem")
		Expect(os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600)).To(Succeed())
		cfg.actorJWTCAFile = notPEM
		Expect(run(specCtx, cfg)).To(MatchError(ContainSubstring("no PEM certificate")))
		bad := cfg
		bad.actorJWTIssuer = "http://idp.ate-system.svc"
		Expect(run(specCtx, bad)).To(MatchError(ContainSubstring("https URL")))

		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		ca := filepath.Join(GinkgoT().TempDir(), "ca.pem")
		Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
		cfg.actorJWTCAFile = ca

		ctx, term := context.WithCancel(context.Background())
		DeferCleanup(term)
		done := make(chan error, 1)
		go func() { done <- run(ctx, cfg) }()
		get := func(tok string) (int, string) {
			req, err := http.NewRequestWithContext(specCtx, http.MethodGet, "http://"+cfg.egressAddr+"/egress/registry.example.test/x", nil)
			Expect(err).NotTo(HaveOccurred())
			if tok != "" {
				req.Header.Set(route.TokenHeader, tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, ""
			}
			_ = resp.Body.Close()
			return resp.StatusCode, resp.Header.Get(egress.RefusalHeader)
		}
		Eventually(func() int { c, _ := get(""); return c }, racescale.D(10*time.Second), 50*time.Millisecond).
			Should(Equal(http.StatusUnauthorized))
		code, reason := get(actortoken.Placeholder)
		Expect([]any{code, reason}).To(Equal([]any{http.StatusUnauthorized, string(route.ReasonBadToken)}))
		// A well-formed token makes the verifier read the issuer's keys; the issuer is down, so it refuses.
		code, reason = get(unsignedLookingJWT())
		Expect([]any{code, reason}).To(Equal([]any{http.StatusServiceUnavailable, string(route.ReasonIdentityUnavailable)}))
		term()
		Eventually(done, racescale.D(10*time.Second)).Should(Receive(BeNil()))
	}, SpecTimeout(40*time.Second))

	It("refuses the MCP listener's address and a bad allowed host", func() {
		on := []string{"--actor-token-issuer", actorIssuer, "--actor-token-keys-dir", keysDir, "--namespace", "koinon"}
		Expect(cli.Check(NewRootCommand(), base(append(on, "--egress-addr", ":8080", "--listen-addr", ":8080")...), nil)).
			To(MatchError(ContainSubstring("must differ")))
		Expect(cli.Check(NewRootCommand(), base(append(on, "--egress-addr", ":8090", "--egress-allowed-hosts", "*.svc")...), nil)).
			To(MatchError(ContainSubstring("egress-allowed-hosts")))
	})

	It("refuses to start without a usable key directory or with a bad route file", func(ctx SpecContext) {
		cfg := config{
			mcpPath: true, stateURL: stateURL, stateTokenFile: tokenFile, listenAddr: "127.0.0.1:0", drain: time.Second,
			egressAddr: "127.0.0.1:0", actorTokenVerifier: verifierEd25519, actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, namespace: "koinon",
		}
		Expect(run(ctx, cfg)).To(MatchError(ContainSubstring("actortoken")), "an empty key directory")

		writeActorKey(keysDir, "k1")
		bad := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(bad, []byte(`{"routes":[{"name":"a","class":"registry","org":"acme","host":"pypi.org","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())
		cfg.egressRoutesFile = bad
		Expect(run(ctx, cfg)).To(MatchError(ContainSubstring("unknown field")))
	}, SpecTimeout(20*time.Second))

	It("adds the roots of --egress-ca-file to the system roots, and refuses a file without a certificate (#1086 part C2)", func(ctx SpecContext) {
		pool, err := egressRoots("")
		Expect(err).NotTo(HaveOccurred())
		Expect(pool).To(BeNil(), "the system roots")

		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		DeferCleanup(srv.Close)
		ca := filepath.Join(GinkgoT().TempDir(), "ca.pem")
		Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
		pool, err = egressRoots(ca)
		Expect(err).NotTo(HaveOccurred())
		_, err = srv.Certificate().Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"})
		Expect(err).NotTo(HaveOccurred(), "the private root verifies its server")

		empty := filepath.Join(GinkgoT().TempDir(), "empty.pem")
		Expect(os.WriteFile(empty, []byte("not a certificate\n"), 0o600)).To(Succeed())
		_, err = egressRoots(empty)
		Expect(err).To(MatchError(ContainSubstring("no PEM certificate")))
		_, err = egressRoots(filepath.Join(GinkgoT().TempDir(), "missing.pem"))
		Expect(err).To(MatchError(ContainSubstring("--egress-ca-file")))

		writeActorKey(keysDir, "k1")
		cfg := config{
			mcpPath: true, stateURL: stateURL, stateTokenFile: tokenFile, listenAddr: "127.0.0.1:0", drain: time.Second,
			egressAddr: "127.0.0.1:0", actorTokenVerifier: verifierEd25519, actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, namespace: "koinon", egressCAFile: empty,
		}
		Expect(run(ctx, cfg)).To(MatchError(ContainSubstring("--egress-ca-file")), "a bad file stops the start")
	}, SpecTimeout(20*time.Second))

	It("serves the platform routes on their own listener, refuses what no route allows and drains on the signal", func(specCtx SpecContext) {
		minter := writeActorKey(keysDir, "k1")
		routes := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(routes, []byte(`{"routes":[{"name":"npm","class":"registry","host":"registry.npmjs.org","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())

		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), egressAddr: freeAddr(),
			mcpPath: true, stateURL: "http://" + freeAddr(), stateTokenFile: tokenFile, podName: "koinon-gateway-0",
			drain: 5 * time.Second, shutdownDelay: time.Second,
			actorTokenVerifier: verifierEd25519, actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, egressRoutesFile: routes, namespace: "koinon",
		}
		ctx, term := context.WithCancel(context.Background())
		DeferCleanup(term)
		done := make(chan error, 1)
		go func() { done <- run(ctx, cfg) }()

		token, err := minter.Mint(actortoken.Actor{Org: "acme", Agent: "repo-helper"}, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		get := func(path, tok string) (int, string) {
			req, err := http.NewRequestWithContext(specCtx, http.MethodGet, "http://"+cfg.egressAddr+path, nil)
			Expect(err).NotTo(HaveOccurred())
			if tok != "" {
				req.Header.Set(route.TokenHeader, tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, ""
			}
			_ = resp.Body.Close()
			return resp.StatusCode, resp.Header.Get(egress.RefusalHeader)
		}
		Eventually(func() int { c, _ := get("/egress/registry.npmjs.org/left-pad", ""); return c }, racescale.D(10*time.Second), 50*time.Millisecond).
			Should(Equal(http.StatusUnauthorized))
		code, reason := get("/egress/registry.npmjs.org/left-pad", "")
		Expect(reason).To(Equal(string(route.ReasonNoToken)))
		Expect(code).To(Equal(http.StatusUnauthorized))
		code, _ = get("/egress/evil.example.test/x", token)
		Expect(code).To(Equal(http.StatusForbidden), "a host with no route")
		// The MCP listener does not serve the egress path.
		resp, err := http.Get("http://" + cfg.listenAddr + "/egress/registry.npmjs.org/left-pad")
		if err == nil {
			_ = resp.Body.Close()
			Expect(resp.Header.Get(egress.RefusalHeader)).To(BeEmpty())
		}

		// The spec never sends an allowed request: that would dial the real registry. During the
		// shutdown delay the listener still serves, so a host with no route gets no_route. After the
		// drain the listener is closed. (The package egress specs prove the draining refusal.)
		term()
		Consistently(func() string { _, r := get("/egress/evil.example.test/x", token); return r }, 200*time.Millisecond, 20*time.Millisecond).
			Should(Equal(string(route.ReasonNoRoute)))
		Eventually(done, racescale.D(10*time.Second)).Should(Receive(BeNil()))
		code, _ = get("/egress/evil.example.test/x", token)
		Expect(code).To(Equal(0), "the listener is closed after the drain")
	}, SpecTimeout(40*time.Second))

	// ADR 0107: a held gateway is ready but deny-all, on the egress listener too (issue #1390). The
	// hold is read at start, so the release is a start without --restore-hold.
	It("refuses every egress request while held, also with --egress-routes-file, and serves again after the release", func(specCtx SpecContext) {
		minter := writeActorKey(keysDir, "k1")
		// A reserved host: a regression that serves while held fails to dial and never reaches the internet.
		routes := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(routes, []byte(`{"routes":[{"name":"reg","class":"registry","host":"registry.example.test","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())
		token, err := minter.Mint(actortoken.Actor{Org: "acme", Agent: "repo-helper"}, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		start := func(held bool, routesFile string) (string, func()) {
			cfg := config{
				listenAddr: freeAddr(), healthAddr: freeAddr(), egressAddr: freeAddr(),
				mcpPath: true, stateURL: "http://" + freeAddr(), stateTokenFile: tokenFile, podName: "koinon-gateway-0",
				drain: time.Second, actorTokenVerifier: verifierEd25519, actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, egressRoutesFile: routesFile,
				namespace: "koinon", restoreHold: held,
			}
			ctx, term := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- run(ctx, cfg) }()
			stop := func() {
				term()
				Eventually(done, racescale.D(10*time.Second)).Should(Receive(BeNil()))
			}
			DeferCleanup(term)
			return cfg.egressAddr, stop
		}
		get := func(addr, path, tok string) (int, string, string) {
			req, err := http.NewRequestWithContext(specCtx, http.MethodGet, "http://"+addr+path, nil)
			Expect(err).NotTo(HaveOccurred())
			if tok != "" {
				req.Header.Set(route.TokenHeader, tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, "", ""
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header.Get(egress.RefusalHeader), string(b)
		}
		allowed := "/egress/registry.example.test/left-pad"

		for _, routesFile := range []string{routes, ""} {
			By("held, routes file " + strconv.Quote(routesFile))
			addr, stop := start(true, routesFile)
			Eventually(func() int { c, _, _ := get(addr, allowed, token); return c }, racescale.D(10*time.Second), 50*time.Millisecond).
				Should(Equal(http.StatusServiceUnavailable))
			// The request that a platform route allows, one with no token and one with no route: all held.
			for _, c := range []struct{ path, tok string }{{allowed, token}, {allowed, ""}, {"/egress/evil.example.test/x", token}} {
				code, reason, body := get(addr, c.path, c.tok)
				Expect(code).To(Equal(http.StatusServiceUnavailable), c.path)
				Expect(reason).To(Equal(string(egress.ReasonHeld)), c.path)
				Expect(body).To(ContainSubstring(restorehold.Message), c.path)
			}
			stop()
		}

		By("released: the same config without --restore-hold serves the routes again")
		addr, stop := start(false, routes)
		// The gate answers with its own reasons, so the request passed the hold.
		Eventually(func() string { _, r, _ := get(addr, allowed, ""); return r }, racescale.D(10*time.Second), 50*time.Millisecond).
			Should(Equal(string(route.ReasonNoToken)))
		_, reason, _ := get(addr, "/egress/evil.example.test/x", token)
		Expect(reason).To(Equal(string(route.ReasonNoRoute)))
		stop()
	}, SpecTimeout(60*time.Second))
})

// unsignedLookingJWT is a compact ES256 JWS with a kid and a zero signature: well formed, so the
// Substrate verifier reads the issuer's keys before it can refuse it. It verifies under no key.
func unsignedLookingJWT() string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"ES256","kid":"koinon-test-kid","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(`{"sub":"actor/koinon-test/koinon-test"}`))
	return header + "." + payload + "." + enc.EncodeToString(make([]byte, 64))
}
