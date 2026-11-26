// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
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
	withEgress := func(c *agentd.Config) {
		c.ActorTokens, c.EgressURL, c.ActorTokenRefresh = minter(), egressURL, 100*time.Millisecond
	}
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

	Describe("the wait for koinon-gateway to serve the agent's forge credential", func() {
		// record writes the agent's gateway_agent_forge row, as koinon-reconciler does.
		record := func(ctx context.Context, e *env, rev int64, served bool, failure string) {
			GinkgoHelper()
			_, err := db.Exec(ctx, `INSERT INTO gateway_agent_forge (org_id, agent_id, state_revision, served_at, failure)
				VALUES ($1, $2, NULLIF($3::bigint, 0), CASE WHEN $4::bool THEN now() END, $5)
				ON CONFLICT (org_id, agent_id) DO UPDATE SET state_revision = EXCLUDED.state_revision,
				served_at = EXCLUDED.served_at, failure = EXCLUDED.failure`, e.org, e.agent.ID, rev, served, failure)
			Expect(err).NotTo(HaveOccurred())
		}
		waits := func(ctx context.Context, r store.Run) func() []store.RunEvent {
			return func() []store.RunEvent { return eventOf(events(ctx, r), "run.waiting_for_forge") }
		}

		It("starts at once without a record; a new credential on the same agent holds the next run until it is served", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
			first := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withEgress, func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
			Eventually(runState(ctx, first)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(waits(ctx, first)()).To(BeEmpty(), "no record: the agent expects no credential")

			By("the agent gets a new grant: its ready sandbox stays, the reconciler records the credential as pending")
			record(ctx, e, 0, false, "")
			execs := len(e.gw.Execs())
			next := e.enqueue(ctx, e.task(ctx, "again"))
			Eventually(waits(ctx, next)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
			Expect(payload(waits(ctx, next)()[0])).To(HaveKeyWithValue("reason", "waiting for koinon-gateway to serve the agent's code-host credential"))
			record(ctx, e, 7, false, "")
			Consistently(runState(ctx, next)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting),
				"a revision that a live copy has not acked holds the run")
			Expect(waits(ctx, next)()).To(HaveLen(1), "the wait is recorded once")

			record(ctx, e, 7, true, "")
			Eventually(runState(ctx, next)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(len(e.gw.Execs())).To(BeNumerically(">", execs))
		})

		It("fails the run with the reconciler's cause, and the harness never starts", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
			record(ctx, e, 0, false, "koinon-reconciler has no forge credential for the agent: mint the code-host token: suspended")
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withEgress)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(getRun(ctx, r).OutcomeReason.String).To(HavePrefix("koinon-reconciler has no forge credential for the agent: mint the code-host token: suspended; "))
			Expect(waits(ctx, r)()).To(BeEmpty())
			_, _, _, args := e.harness.snapshot()
			Expect(args).To(BeEmpty(), "the harness never starts")
		})

		It("fails the run when the credential is not served in time", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
			record(ctx, e, 0, false, "")
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a", withEgress, func(c *agentd.Config) { c.ForgeServeTimeout = 700 * time.Millisecond })
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
			Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("koinon-gateway did not serve the agent's code-host credential within 700ms"))
			Expect(waits(ctx, r)()).To(HaveLen(1))
			_, _, _, args := e.harness.snapshot()
			Expect(args).To(BeEmpty(), "the harness never starts")
		})

		It("does not read the record of an agent that does not clone through the gateway", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, ghesAgent(ctx))
			record(ctx, e, 0, false, "")
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Expect(waits(ctx, r)()).To(BeEmpty())
		})
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
