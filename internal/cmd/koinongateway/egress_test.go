// SPDX-License-Identifier: Apache-2.0

package koinongateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
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
			egressAddr: "127.0.0.1:0", actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, namespace: "koinon",
		}
		Expect(run(ctx, cfg)).To(MatchError(ContainSubstring("actortoken")), "an empty key directory")

		writeActorKey(keysDir, "k1")
		bad := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(bad, []byte(`{"routes":[{"name":"a","class":"registry","org":"acme","host":"pypi.org","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())
		cfg.egressRoutesFile = bad
		Expect(run(ctx, cfg)).To(MatchError(ContainSubstring("unknown field")))
	}, SpecTimeout(20*time.Second))

	It("serves the platform routes on their own listener, refuses what no route allows and drains on the signal", func(specCtx SpecContext) {
		minter := writeActorKey(keysDir, "k1")
		routes := filepath.Join(GinkgoT().TempDir(), "routes.json")
		Expect(os.WriteFile(routes, []byte(`{"routes":[{"name":"npm","class":"registry","host":"registry.npmjs.org","methods":["GET"],"pathPrefixes":["/"]}]}`), 0o600)).To(Succeed())

		cfg := config{
			listenAddr: freeAddr(), healthAddr: freeAddr(), egressAddr: freeAddr(),
			mcpPath: true, stateURL: "http://" + freeAddr(), stateTokenFile: tokenFile, podName: "koinon-gateway-0",
			drain: 5 * time.Second, shutdownDelay: time.Second,
			actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, egressRoutesFile: routes, namespace: "koinon",
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
				drain: time.Second, actorTokenIssuer: actorIssuer, actorTokenKeysDir: keysDir, egressRoutesFile: routesFile,
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
