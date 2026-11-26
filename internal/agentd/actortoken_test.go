// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

var _ = Describe("git through the gateway's egress prefix (#1086 part C2)", func() {
	const egressURL = "http://koinon-mcp-gateway.koinon.svc:8081"
	// An obviously fake seed: 32 bytes of 0x01.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 1
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub, _ := key.Public().(ed25519.PublicKey)

	minter := func() *actortoken.Minter {
		m, err := actortoken.NewMinter("koinon-agentd", "k1", key)
		Expect(err).NotTo(HaveOccurred())
		return m
	}
	verifier := func() *actortoken.Verifier {
		v, err := actortoken.NewVerifier("koinon-agentd", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
		return v
	}
	// withProbe turns git through the gateway on, with p as the gateway's egress listener.
	withProbe := func(p *fakeProbe) func(c *agentd.Config) {
		return func(c *agentd.Config) {
			c.ActorTokens, c.EgressURL, c.ActorTokenRefresh = minter(), egressURL, 100*time.Millisecond
			c.EgressProbe = p
		}
	}
	// withEgress is withProbe with a gateway that serves the forge route at once.
	withEgress := func(c *agentd.Config) { withProbe(&fakeProbe{})(c) }
	// ghesAgent binds the agent to a GitHub connection on ghes.example.test.
	ghesAgent := func(ctx context.Context) func(e *env) string {
		return func(e *env) string {
			connID := uuid.New()
			cfg, err := json.Marshal(connections.GitHubConfig{AppID: 1, InstallationID: 2, Site: codehost.Site{BaseURL: "https://ghes.example.test"}})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
				OrgID: e.org, ID: pgtype.UUID{Bytes: connID, Valid: true}, Connector: "github", DisplayName: "ghes",
				Config: cfg, SecretEnvelope: []byte(`{}`), Source: store.ConfigSourceUi,
			})
			Expect(err).NotTo(HaveOccurred())
			return `{"instructions":"help","model":"` + model + `",` +
				`"grants":[{"connector":"github","scopes":["repo.read"],"preset":"standard"}],` +
				`"bindings":[{"connector":"github","connectionId":"` + connID.String() + `","targets":["acme/widgets"]}]}`
		}
	}
	tokenOf := func(b []byte) string {
		GinkgoHelper()
		_, after, ok := strings.Cut(string(b), "extraHeader = "+codehost.ActorTokenHeader+": ")
		Expect(ok).To(BeTrue(), string(b))
		return strings.TrimSpace(after)
	}

	It("writes a valid actor token outside the home, rewrites the code host to the gateway, renews the token and removes it at the end", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
		// Closing the harness takes two seconds: long enough for several renewals.
		e.harness.set(func(h *fakeHarness) { h.lingers = true })
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", withEgress)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))

		cfg, ok := e.files.get(codehost.SandboxGitConfig)
		Expect(ok).To(BeTrue())
		Expect(string(cfg)).To(ContainSubstring("[url \"" + egressURL + "/egress/ghes.example.test/\"]\n\tinsteadOf = https://ghes.example.test/\n"))
		Expect(string(cfg)).To(ContainSubstring("path = " + codehost.SandboxEgressToken))
		Expect(string(cfg)).NotTo(ContainSubstring("eyJ"), "the home file holds no token")
		Expect(codehost.SandboxEgressToken).NotTo(HavePrefix("/sandbox/"), "the token file is outside the checkpointed home")

		var hist [][]byte
		Eventually(func() [][]byte { hist = e.files.history(codehost.SandboxEgressToken); return hist }).
			WithTimeout(racescale.D(10 * time.Second)).Should(WithTransform(func(h [][]byte) int { return len(h) }, BeNumerically(">=", 3)))
		Eventually(func() []byte {
			h := e.files.history(codehost.SandboxEgressToken)
			return h[len(h)-1]
		}).WithTimeout(racescale.D(10*time.Second)).Should(BeNil(), "removed when the run ends")
		_, present := e.files.get(codehost.SandboxEgressToken)
		Expect(present).To(BeFalse())

		hist = e.files.history(codehost.SandboxEgressToken)
		seen := map[string]bool{}
		for _, b := range hist[:len(hist)-1] {
			Expect(b).NotTo(BeNil())
			Expect(string(b)).To(ContainSubstring("[http \"" + egressURL + "/egress/\"]"))
			tok := tokenOf(b)
			Expect(seen[tok]).To(BeFalse(), "each renewal is a new token")
			seen[tok] = true
			c, err := verifier().Verify(tok)
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Actor.Agent).To(Equal(uuid.UUID(e.agent.ID.Bytes).String()))
			Expect(c.Actor.Run).To(Equal(uuid.UUID(r.ID.Bytes).String()))
			var slug string
			Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, e.org).Scan(&slug)).To(Succeed())
			Expect(c.Actor.Org).To(Equal(slug))
			Expect(c.ExpiresAt.Sub(c.IssuedAt)).To(Equal(15 * time.Minute))
		}
	})

	Describe("the wait for the gateway's forge route", func() {
		const probeURL = egressURL + "/egress/ghes.example.test/acme/widgets.git/info/refs?service=git-upload-pack"

		It("waits while the forge credential is still reaching the gateway, then starts the harness", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
			p := &fakeProbe{refusal: "credential_unavailable"}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withProbe(p))
			Eventually(p.count).WithTimeout(racescale.D(10 * time.Second)).Should(BeNumerically(">=", 2))
			Consistently(runState(ctx, r)).WithTimeout(300*time.Millisecond).Should(Equal(store.RunStateStarting),
				"the harness does not start while the route answers credential_unavailable")
			p.set("")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))

			for _, req := range p.requests() {
				Expect(req.Method).To(Equal(http.MethodGet))
				Expect(req.URL).To(Equal(probeURL), "the first request of git's clone of the bound repository, through the egress prefix")
				Expect(req.Authorization).To(BeEmpty(), "koinon-agentd sends no forge credential")
				c, err := verifier().Verify(req.Token)
				Expect(err).NotTo(HaveOccurred())
				Expect(c.Actor.Run).To(Equal(uuid.UUID(r.ID.Bytes).String()))
				Expect(c.ExpiresAt.Sub(c.IssuedAt)).To(Equal(actortoken.MinTTL), "the probe's own token has the shortest lifetime")
			}
		})

		DescribeTable("waits for the refusals that a reconciler pass ends, and for a gateway it cannot reach",
			func(ctx SpecContext, refusal string, fail bool) {
				e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
				p := &fakeProbe{refusal: refusal, fail: fail}
				r := e.enqueue(ctx, e.task(ctx, "hello"))
				e.pool("agentd-a", withProbe(p), func(c *agentd.Config) { c.ForgeServeTimeout = 700 * time.Millisecond })
				Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
				want := refusal
				if fail {
					want = "unreachable"
				}
				Expect(getRun(ctx, r).OutcomeReason.String).To(And(
					ContainSubstring("the Koinon gateway did not serve the forge route of the github host ghes.example.test within 700ms"),
					ContainSubstring("(its egress listener answers "+want+")"),
				))
				Expect(getRun(ctx, r).OutcomeReason.String).NotTo(ContainSubstring("eyJ"), "the reason holds no token")
			},
			Entry("credential_unavailable", "credential_unavailable", false),
			Entry("no_table", "no_table", false),
			Entry("no_route", "no_route", false),
			Entry("draining", "draining", false),
			Entry("a transport error", "", true),
		)

		DescribeTable("ends the wait on an answer that no wait changes; the gateway still gates git",
			func(ctx SpecContext, refusal string) {
				e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
				p := &fakeProbe{refusal: refusal}
				r := e.enqueue(ctx, e.task(ctx, "hello"))
				e.pool("agentd-a", withProbe(p))
				Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
				Expect(p.count()).To(Equal(1))
			},
			Entry("the forge's own answer", ""),
			Entry("the upstream could not be reached", "upstream_error"),
			Entry("a path the route does not allow", "path_not_allowed"),
		)

		It("does not wait for an agent without a bound repository", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, func(e *env) string {
				return strings.Replace(ghesAgent(ctx)(e), `"targets":["acme/widgets"]`, `"targets":[]`, 1)
			})
			p := &fakeProbe{refusal: "credential_unavailable"}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withProbe(p))
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(p.count()).To(BeZero())
		})
	})

	It("leaves git on the driver's egress for an agent without a code-host connection", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", withEgress)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		cfg, _ := e.files.get(codehost.SandboxGitConfig)
		Expect(string(cfg)).NotTo(ContainSubstring("/egress/"))
		Expect(e.files.history(codehost.SandboxEgressToken)).To(BeEmpty())
	})

	It("leaves git on the driver's egress without a minter", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		cfg, _ := e.files.get(codehost.SandboxGitConfig)
		Expect(string(cfg)).NotTo(ContainSubstring("/egress/"))
		Expect(e.files.history(codehost.SandboxEgressToken)).To(BeEmpty())
	})

	DescribeTable("refuses egress settings that do not go together or that would leak into the sandbox",
		func(mutate func(*agentd.Config)) {
			c := agentd.Config{OwnerID: "agentd-a", GatewayURL: "http://gw:8080"}
			mutate(&c)
			_, err := agentd.New(nil, nil, c)
			Expect(err).To(MatchError(ContainSubstring("Egress")))
		},
		Entry("a minter without a URL", func(c *agentd.Config) { c.ActorTokens = minter() }),
		Entry("a URL without a minter", func(c *agentd.Config) { c.EgressURL = egressURL }),
		Entry("user info", func(c *agentd.Config) { c.ActorTokens, c.EgressURL = minter(), "http://u:p@gw:8081" }),
		Entry("a path", func(c *agentd.Config) { c.ActorTokens, c.EgressURL = minter(), "http://gw:8081/x" }),
		Entry("a query", func(c *agentd.Config) { c.ActorTokens, c.EgressURL = minter(), "http://gw:8081?a=b" }),
		Entry("a quote", func(c *agentd.Config) { c.ActorTokens, c.EgressURL = minter(), "http://gw:8081\"" }),
		Entry("another scheme", func(c *agentd.Config) { c.ActorTokens, c.EgressURL = minter(), "ftp://gw:8081" }),
		Entry("a renewal too slow for the lifetime", func(c *agentd.Config) {
			c.ActorTokens, c.EgressURL, c.ActorTokenRefresh = minter(), egressURL, 6*time.Minute
		}),
	)
})

// probeRequest is what the fake egress listener saw of one probe.
type probeRequest struct {
	Method, URL, Token, Authorization string
}

// fakeProbe is the gateway's egress listener for koinon-agentd's forge probe: it answers with the
// refusal header refusal (none when empty), or with a transport error when fail is set.
type fakeProbe struct {
	mu      sync.Mutex
	refusal string
	fail    bool
	reqs    []probeRequest
}

func (p *fakeProbe) Do(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, probeRequest{
		Method: req.Method, URL: req.URL.String(), Token: req.Header.Get(codehost.ActorTokenHeader),
		Authorization: req.Header.Get("Authorization"),
	})
	if p.fail {
		return nil, errors.New("dial tcp: connection refused")
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
	if p.refusal != "" {
		resp.StatusCode = http.StatusServiceUnavailable
		resp.Header.Set("Koinon-Egress-Refusal", p.refusal)
	}
	return resp, nil
}

func (p *fakeProbe) set(refusal string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refusal = refusal
}

func (p *fakeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *fakeProbe) requests() []probeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]probeRequest(nil), p.reqs...)
}
