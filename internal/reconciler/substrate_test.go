// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/guestlaunch"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

// substrateImage is pinned by digest: the Substrate driver refuses any other reference. The digest
// is a fake, the fakeate never pulls.
const substrateImage = "registry.example.com/koinon/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// The reconciler against the fake Substrate (issue #1245): the spec it renders must be one that the
// Substrate driver accepts, as the production wiring (execution.WithoutEgress around the driver) gives it.
var _ = Describe("Reconciler on the Substrate driver", func() {
	var (
		cl   *fakeate.Cluster
		d    *substrate.Driver
		org  pgtype.UUID
		user pgtype.UUID
		r    *reconciler.Reconciler
		// newR makes a second reconciler on the same backend and database: a leader change.
		newR func() *reconciler.Reconciler
		now  atomic.Pointer[time.Time]
		ws   string
		ctl  ateapiv1.ControlClient
		// checkpoints turns home checkpoints on for the reconcilers that newR makes after it is set.
		checkpoints bool
		// metrics are the metrics of the reconcilers that newR makes.
		metrics *reconciler.Metrics
		// newDriver makes the driver on the fake with cfg added to the specs' common configuration.
		newDriver func(front time.Duration) *substrate.Driver
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	advance := func(d time.Duration) {
		t := time.Now().Add(d)
		now.Store(&t)
	}
	// sandboxRow is the agent's sandbox row; its ActorRef names the current actor (issue #1386).
	sandboxRow := func(ctx context.Context, agent pgtype.UUID) store.AgentSandbox {
		GinkgoHelper()
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent})
		Expect(err).NotTo(HaveOccurred())
		return sb
	}
	actorOf := func(ctx context.Context, agent pgtype.UUID) string { return sandboxRow(ctx, agent).ActorRef() }
	forgeRows := func(ctx context.Context, agent pgtype.UUID) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_agent_forge WHERE org_id = $1 AND agent_id = $2", org, agent).Scan(&n)).To(Succeed())
		return n
	}
	// stateActors are the actors of agent in the newest stored gateway State.
	stateActors := func(ctx context.Context, agent pgtype.UUID) []gateway.EgressActor {
		GinkgoHelper()
		u, err := staterev.Latest(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		var mine []gateway.EgressActor
		for _, a := range staterev.FromProto(u).EgressActors {
			if a.Agent == execution.UUIDString(agent.Bytes) {
				mine = append(mine, a)
			}
		}
		return mine
	}

	BeforeEach(func(ctx SpecContext) {
		cl = fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		ctl = ateapiv1.NewControlClient(control)
		newDriver = func(front time.Duration) *substrate.Driver {
			return substrate.New(control, router, substrate.Config{
				PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond, FrontCheckTimeout: front,
				GatewayRoutes: []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)},
				CodeHostEgressRoute: func() *substrate.GatewayRoute {
					r := substrate.EgressRoute(fakeate.GatewayHost, fakeate.EgressPort)
					return &r
				}(),
				GuestSigner: fakeate.Signer(),
				// The class the reconciler requests (reconciler.Config.RuntimeClassName) has a tier.
				RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM},
			})
		}
		d = newDriver(0)
		metrics = reconciler.NewMetrics(nil)
		now.Store(nil)
		checkpoints = false
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		newR = func() *reconciler.Reconciler {
			rr, err := reconciler.New(pool, execution.WithoutEgress(d), &fakeModelGateway{},
				reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
					Prices:         prices.NewResolver(pricing.Embedded()),
					Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
					Egress:         &reconciler.Endpoint{Host: fakeate.GatewayHost, Port: fakeate.EgressPort},
					GitHub:         fixedTokens{},
					Keys:           ring,
					ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
					ProviderKeyDir: "/secrets/providers",
					URLPolicy:      testPolicy,
					Harnesses: map[string]reconciler.Harness{
						harness.OpenCode: {Image: substrateImage, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
					},
					RuntimeClassName: "kata",
					MCPAckWait:       300 * time.Millisecond, MCPAckPoll: 20 * time.Millisecond,
					KeepWarmPerOrg: 2,
					Checkpoints:    checkpoints,
					Metrics:        metrics,
					Logger:         slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
					Now: func() time.Time {
						if t := now.Load(); t != nil {
							return *t
						}
						return time.Now()
					},
				})
			Expect(err).NotTo(HaveOccurred())
			return rr
		}
		r = newR()
	})

	It("creates an actor, suspends it when idle and resumes it for a run", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		actor := actorOf(ctx, agent.ID)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, actor) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		Expect(cl.Calls("CreateActor")).To(Equal(1))

		By("an idle sandbox is suspended")
		coolDown(ctx, org, agent.ID)
		advance(9 * time.Minute)
		pass(ctx)
		Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		advance(11 * time.Minute)
		pass(ctx)
		Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

		By("a queued run resumes it")
		now.Store(nil)
		queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, actor) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		Expect(cl.Calls("CreateActor")).To(Equal(1), "resumed, not re-created")
		Expect(actorOf(ctx, agent.ID)).To(Equal(actor), "a resume keeps the actor and its name")
	})

	It("the adapter drops provider records, network rules and a filesystem policy; the plain driver refuses them", func(ctx SpecContext) {
		withEgress := execution.SandboxSpec{
			Name: "egress-probe", Image: substrateImage, RuntimeClassName: "kata", Providers: []string{"upstream"},
			CPULimit: "500m", MemoryLimit: "512Mi",
			Policy: execution.Policy{
				Filesystem: execution.FilesystemPolicy{ReadWrite: []string{"/work"}},
				Network:    map[string]execution.NetworkRule{"github": {Binaries: []string{"/usr/bin/git"}}},
			},
		}
		_, err := d.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = d.CreateSandbox(ctx, ws, withEgress)
		Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "plain driver: %v", err)
		_, err = execution.WithoutEgress(d).CreateSandbox(ctx, ws, withEgress)
		Expect(err).NotTo(HaveOccurred())
	})

	// The egress listener follows the code-host grant (issue #1444), as the OpenShell policy entry does, and a
	// change of the grant updates the actor's policy in place.
	It("gives the egress listener only to an agent with a code-host grant, and takes it away in place", func(ctx SpecContext) {
		gw, gwPort, egressPort := fakeate.GatewayHost, fakeate.GatewayPort, fakeate.EgressPort
		plain := newAgent(ctx, org, user)

		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		envelope, err := connections.SealFields(ring, org, id, map[string][]byte{connections.GitHubPrivateKeyField: []byte("unused")})
		Expect(err).NotTo(HaveOccurred())
		connCfg, err := json.Marshal(map[string]any{"appId": 1, "installationId": 2})
		Expect(err).NotTo(HaveOccurred())
		conn, err := q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "github", Config: connCfg,
			SecretEnvelope: envelope, Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		withGrant, err := agentconfig.Config{
			Instructions: "help",
			Grants:       []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings:     []agentconfig.Binding{{Connector: "github", ConnectionID: uuidOf(conn.ID), Targets: []string{"acme/widgets"}}},
		}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		forge := newAgentWithConfig(ctx, org, user, string(withGrant))

		pass(ctx)
		plainActor, forgeActor := actorOf(ctx, plain.ID), actorOf(ctx, forge.ID)
		for _, n := range []string{plainActor, forgeActor} {
			Eventually(func() ateapiv1.ActorState { return cl.State(ws, n) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		}
		Expect(cl.Calls("ResumeWithoutEgressPolicy")).To(BeZero())
		Expect(cl.EgressAllowed(ws, plainActor, gw, gwPort)).To(BeTrue())
		Expect(cl.EgressAllowed(ws, plainActor, gw, egressPort)).To(BeFalse(), "an actor without a code-host grant has no route to the egress listener")
		Expect(cl.EgressAllowed(ws, forgeActor, gw, gwPort)).To(BeTrue())
		Expect(cl.EgressAllowed(ws, forgeActor, gw, egressPort)).To(BeTrue(), "the agent with the grant")
		// Issue #1489: the gateway checks the actor JWT on the model and MCP listeners too, so on Substrate
		// every active agent has a row that waits for its actor's revision, also without a forge grant.
		Expect(forgeRows(ctx, plain.ID)).To(Equal(1), "an actor-only row, for the model and MCP listeners")
		// The first pass records the actor's UID; the State carries the actor from the next pass.
		pass(ctx)
		var rev pgtype.Int8
		var served bool
		var failure string
		Expect(pool.QueryRow(ctx, "SELECT state_revision, served_at IS NOT NULL, failure FROM gateway_agent_forge WHERE org_id = $1 AND agent_id = $2", org, plain.ID).Scan(&rev, &served, &failure)).To(Succeed())
		Expect(rev.Valid && rev.Int64 > 0).To(BeTrue(), "the row waits for the revision that carries the actor")
		Expect(failure).To(BeEmpty())
		Expect(served).To(BeTrue(), "the fake gateway acked the revision")
		Expect(forgeRows(ctx, forge.ID)).To(Equal(1))
		creates, deletes := cl.Calls("CreateActor"), cl.Calls("DeleteActor")

		By("a new version without the grant takes the listener away on the next pass, without a new sandbox")
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: forge.ID, Config: []byte(`{"instructions":"help"}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: forge.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(cl.EgressAllowed(ws, forgeActor, gw, egressPort)).To(BeFalse())
		Expect(cl.EgressAllowed(ws, forgeActor, gw, gwPort)).To(BeTrue())
		Expect(cl.Calls("CreateActor")).To(Equal(creates), "the grant is not part of the sandbox spec")
		Expect(cl.Calls("DeleteActor")).To(Equal(deletes))
		Expect(forgeRows(ctx, forge.ID)).To(Equal(1), "the row stays, for the actor only (issue #1489)")
		Expect(cl.State(ws, forgeActor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
	})

	It("recreates nothing on later passes", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		actor := actorOf(ctx, agent.ID)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, actor) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		deletes := cl.Calls("DeleteActor")
		pass(ctx)
		pass(ctx)
		Expect(cl.Calls("CreateActor")).To(Equal(1), "the Substrate driver keeps no labels: the spec identity must hold across passes")
		Expect(cl.Calls("DeleteActor")).To(Equal(deletes))
		Expect(actorOf(ctx, agent.ID)).To(Equal(actor))
	})

	// Issue #1386: the gateway maps a Substrate actor JWT to an agent by the actor name and UID, so a name
	// never comes back for another actor, and the State carries each actor with its UID.
	It("names each new actor anew, records its UID and carries it in the gateway State", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		pass(ctx)
		first := sandboxRow(ctx, agent.ID)
		Expect(first.SandboxName).To(Equal(names.Sandbox))
		Expect(first.ActorName.Valid).To(BeTrue())
		Expect(first.ActorName.String).To(HavePrefix(names.Sandbox + "-"))
		Expect(first.ActorUid.Valid).To(BeTrue(), "the UID of the created actor is recorded")
		seen, err := d.GetSandbox(ctx, ws, first.ActorName.String)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.ActorUid.String).To(Equal(seen.ID), "the UID is the actor's metadata UID")
		var slug string
		Expect(pool.QueryRow(ctx, "SELECT slug FROM orgs WHERE id = $1", org).Scan(&slug)).To(Succeed())
		Expect(cl.State(ws, names.Sandbox)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "no actor under the sandbox name")

		By("the next pass publishes the actor with its org and agent")
		pass(ctx)
		Expect(stateActors(ctx, agent.ID)).To(Equal([]gateway.EgressActor{{
			Atespace: ws, Name: first.ActorName.String, UID: first.ActorUid.String,
			Org: slug, Agent: execution.UUIDString(agent.ID.Bytes),
		}}))

		By("a crashed actor with no snapshot is replaced by one with a new name and a new UID")
		cl.DropSnapshots(ws, first.ActorName.String) // a revert would give an empty home: the repair path runs
		cl.Crash(ws, first.ActorName.String, "guest kernel panic")
		Eventually(func(g Gomega) string {
			g.Expect(r.ReconcileAll(ctx)).To(Succeed())
			sb := sandboxRow(ctx, agent.ID)
			g.Expect(sb.ActorUid.Valid).To(BeTrue())
			return sb.ActorName.String
		}).ShouldNot(Equal(first.ActorName.String))
		second := sandboxRow(ctx, agent.ID)
		Expect(second.ActorUid.String).NotTo(Equal(first.ActorUid.String))
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, second.ActorName.String) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		pass(ctx)
		Expect(stateActors(ctx, agent.ID)).To(Equal([]gateway.EgressActor{{
			Atespace: ws, Name: second.ActorName.String, UID: second.ActorUid.String,
			Org: slug, Agent: execution.UUIDString(agent.ID.Bytes),
		}}), "only the current actor")
	})

	// Issue #1481: the crash and liveness path.
	Describe("the crash and liveness path", func() {
		running := func(actor string) func() ateapiv1.ActorState {
			return func() ateapiv1.ActorState { return cl.State(ws, actor) }
		}
		It("reverts a crashed actor and keeps it, its name and its UID", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			before := sandboxRow(ctx, agent.ID)
			actor := before.ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
			pass(ctx)
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			after := sandboxRow(ctx, agent.ID)
			Expect(after.ActorName.String).To(Equal(actor), "reverted, not recreated")
			Expect(after.ActorUid.String).To(Equal(before.ActorUid.String))
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(after.Message).To(ContainSubstring("lost: the writes after"))
		})

		// readyActor creates the agent's actor, waits until it runs, and lets one pass see it ready.
		readyActor := func(ctx SpecContext) (store.Agent, string) {
			GinkgoHelper()
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			advance(0)
			pass(ctx)
			return agent, actor
		}

		It("reverts a running actor whose front is silent directly from RUNNING, with no suspend first", func(ctx SpecContext) {
			_, actor := readyActor(ctx)
			cl.KillFront(ws, actor)
			advance(time.Minute)
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(BeZero(), "one failed check is not proof")
			advance(time.Minute + 10*time.Second)
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(BeZero(), "two are not enough, and the silence is 10 s old")
			advance(3 * time.Minute)
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(1), "three checks in a row over the silence window")
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "only the seal of the first boot: a suspend of a dead front would save it over the last good snapshot")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(d.CheckFront(ctx, ws, actor)).To(Succeed(), "the front of the good snapshot answers")
		})

		It("does not revert while the router answers the guest calls itself, and reverts once it reports the dead front", func(ctx SpecContext) {
			_, actor := readyActor(ctx)
			cl.KillFront(ws, actor)
			cl.RouterRefuse(ws, actor, true) // the router's own Unavailable: the guest is not reached
			for i := 1; i <= 5; i++ {
				advance(time.Duration(i) * time.Minute)
				pass(ctx)
			}
			Expect(cl.Calls("RevertActor")).To(BeZero(), "an Unavailable of the router says nothing about the front")
			Expect(cl.Calls("DeleteActor")).To(BeZero())

			By("the router reaches the worker again and reports the dead front")
			cl.RouterRefuse(ws, actor, false)
			for _, off := range []time.Duration{10 * time.Minute, 11 * time.Minute, 12 * time.Minute} {
				advance(off)
				pass(ctx)
			}
			Expect(cl.Calls("RevertActor")).To(Equal(1), "a dead front still reverts")
		})

		It("does not revert a running sandbox during a router outage", func(ctx SpecContext) {
			_, actor := readyActor(ctx)
			cl.StopRouter() // the client has no connection to the router: every guest call fails with Unavailable
			for i := 1; i <= 5; i++ {
				advance(time.Duration(i) * time.Minute)
				_ = r.ReconcileAll(ctx) // other router calls of the pass may fail too
			}
			Expect(cl.Calls("RevertActor")).To(BeZero(), "a router outage says nothing about the front")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		})

		It("leaves a sandbox that was just seen ready alone for the grace time", func(ctx SpecContext) {
			_, actor := readyActor(ctx)
			cl.KillFront(ws, actor)
			for _, off := range []time.Duration{5 * time.Second, 15 * time.Second, 25 * time.Second} {
				advance(off)
				pass(ctx)
			}
			Expect(cl.Calls("RevertActor")).To(BeZero())
		})

		// The fleet breaker (maintainer decision on #1481, 2026-11-28): a router-side failure gives the same
		// verdict as a dead front, so silence on more than one worker in one window reverts nothing.
		Describe("the fleet breaker", func() {
			breakerOpen := func() float64 { return testutil.ToFloat64(metrics.FrontBreakerOpen) }
			trips := func() float64 { return testutil.ToFloat64(metrics.FrontBreakerTrips) }
			// on places a ready actor on a worker.
			on := func(ctx SpecContext, worker string) string {
				GinkgoHelper()
				_, actor := readyActor(ctx)
				cl.SetWorker(ws, actor, worker)
				return actor
			}
			// passes runs one pass at each offset: 20 s apart, inside the breaker window.
			passes := func(ctx SpecContext, offs ...time.Duration) {
				GinkgoHelper()
				for _, off := range offs {
					advance(off)
					pass(ctx)
				}
			}
			silence := []time.Duration{time.Minute, time.Minute + 20*time.Second, time.Minute + 40*time.Second, 2 * time.Minute, 2*time.Minute + 20*time.Second}

			It("(a) still reverts one dead front while the other fronts answer", func(ctx SpecContext) {
				// Two sandboxes: the org's creation rate holds back a third in one spec (issue #27). The share
				// rule is in TestFleetBreaker.
				dead := on(ctx, "worker-a")
				on(ctx, "worker-b")
				cl.KillFront(ws, dead)
				passes(ctx, silence[:3]...)
				Expect(cl.Calls("RevertActor")).To(Equal(1), "one dead front on one worker is a crash")
				Expect(breakerOpen()).To(BeZero())
				Expect(trips()).To(BeZero())
			})

			It("(b) reverts nothing while fronts on two workers are silent, raises the alert once, and (c) gives verdicts again after the outage", func(ctx SpecContext) {
				a := on(ctx, "worker-a")
				b := on(ctx, "worker-b")
				cl.KillFront(ws, a) // the router's upstream failure: both look like a dead front
				cl.KillFront(ws, b)
				passes(ctx, silence...)
				Expect(cl.Calls("RevertActor")).To(BeZero(), "silence on two workers in one window is not a guest crash")
				Expect(cl.Calls("DeleteActor")).To(BeZero())
				Expect(breakerOpen()).To(Equal(1.0))
				Expect(trips()).To(Equal(1.0), "one opening, not one per pass or per sandbox")

				By("the outage ends: the fronts answer and the breaker closes")
				cl.ReviveFront(ws, a)
				cl.ReviveFront(ws, b)
				passes(ctx, 2*time.Minute+40*time.Second)
				Expect(breakerOpen()).To(BeZero())

				By("a front that dies after the outage gets its verdict as before")
				cl.KillFront(ws, a)
				passes(ctx, 3*time.Minute+40*time.Second, 4*time.Minute, 4*time.Minute+20*time.Second)
				Expect(cl.Calls("RevertActor")).To(Equal(1))
				Expect(breakerOpen()).To(BeZero())
				Expect(trips()).To(Equal(1.0))
			})

			It("(e) gives a front that stays silent across the close a full new window after the close", func(ctx SpecContext) {
				a := on(ctx, "worker-a")
				b := on(ctx, "worker-b")
				cl.KillFront(ws, a)
				cl.KillFront(ws, b)
				passes(ctx, silence...)
				Expect(breakerOpen()).To(Equal(1.0))

				By("the router recovers worker-b first: the breaker closes while the front on worker-a is still silent")
				cl.ReviveFront(ws, b)
				passes(ctx, 2*time.Minute+40*time.Second)
				Expect(breakerOpen()).To(BeZero())
				passes(ctx, 3*time.Minute)
				Expect(cl.Calls("RevertActor")).To(BeZero(), "the silence held by the open breaker starts again at the close")

				By("the front stays silent for a full window after the close")
				passes(ctx, 3*time.Minute+20*time.Second, 3*time.Minute+40*time.Second)
				Expect(cl.Calls("RevertActor")).To(Equal(1))
			})

			It("(f) sets the open gauge to 0 when the lead ends", func(ctx SpecContext) {
				a := on(ctx, "worker-a")
				b := on(ctx, "worker-b")
				cl.KillFront(ws, a)
				cl.KillFront(ws, b)
				passes(ctx, silence...)
				Expect(breakerOpen()).To(Equal(1.0))
				r.EndLeadForTest()
				Expect(breakerOpen()).To(BeZero(), "a former leader must not keep the max over the replicas at 1")

				By("the next lead starts with no marks and no silence")
				passes(ctx, 2*time.Minute+40*time.Second, 3*time.Minute, 3*time.Minute+20*time.Second)
				Expect(cl.Calls("RevertActor")).To(BeZero())
				Expect(breakerOpen()).To(Equal(1.0))
			})

			It("(d) holds back the deadline verdict too", func(ctx SpecContext) {
				d = newDriver(300 * time.Millisecond)
				r = newR()
				a := on(ctx, "worker-a")
				b := on(ctx, "worker-b")
				for _, actor := range []string{a, b} {
					cl.KillFront(ws, actor)
					cl.HangFront(ws, actor) // the call runs into its deadline: the other verdict of the driver
				}
				passes(ctx, silence...)
				Expect(cl.Calls("RevertActor")).To(BeZero())
				Expect(breakerOpen()).To(Equal(1.0))
			})
		})

		// restoreHome stands in for agentd: the row points at a checkpoint, then the restore ends.
		restoreHome := func(ctx SpecContext, agent store.Agent, done bool) {
			GinkgoHelper()
			if done {
				Expect(q.ClearSandboxRestore(ctx, store.ClearSandboxRestoreParams{OrgID: org, AgentID: agent.ID})).To(Succeed())
				return
			}
			sb := sandboxRow(ctx, agent.ID)
			cp, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: org, AgentID: agent.ID, CheckpointID: cp.ID})
			Expect(err).NotTo(HaveOccurred())
		}

		It("takes a SYNC suspend of the idle sandbox after a home restore, also after a leader change", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			coolDown(ctx, org, agent.ID) // keep-warm sandboxes are not sealed
			restoreHome(ctx, agent, false)
			pass(ctx)
			restoreHome(ctx, agent, true)
			Expect(sandboxRow(ctx, agent.ID).RestoredCheckpointID.Valid).To(BeTrue(), "the mark is in Postgres")
			r = newR() // the leader changed: no memory of the restore
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING), "the sandbox is not idle yet")
			advance(2 * time.Hour)
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED), "the restored home is snapshotted")
			Expect(cl.Calls("SuspendActor")).To(Equal(2), "the seal of the first boot and the seal of the restore")
			pass(ctx)
			Expect(sandboxRow(ctx, agent.ID).RestoredCheckpointID.Valid).To(BeFalse(), "the stop took the snapshot: no mark")
		})

		It("does not stop a keep-warm sandbox for the seal", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			advance(2 * time.Hour)
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "only the seal of the first boot")
		})

		It("does not stop a sandbox with a run for the seal", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			queueRun(ctx, org, user, agent.ID)
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			advance(2 * time.Hour)
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "only the seal of the first boot")
		})

		It("reverts a crash before the seal and points the restore at the checkpoint again", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			r = newR()
			cl.Crash(ws, actor, "guest kernel panic")
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			got := sandboxRow(ctx, agent.ID)
			Expect(got.RestoreCheckpointID).To(Equal(restored), "the next run restores the home again")
			a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(a.UnhealthyAt.Valid).To(BeFalse())

			By("the pending restore keeps the seal away: the reverted actor is not snapshotted before the restore ran")
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			advance(3 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "only the seal of the first boot")
			Expect(sandboxRow(ctx, agent.ID).RestoredCheckpointID.Valid).To(BeTrue(), "the mark stays until a stop took the snapshot")
		})

		It("does not revert when the restore pointer cannot be written first, and the mark stays", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeFalse())
			// The database refuses the pointer for this org: the write that must come before the revert.
			_, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION refuse_restore_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'restore write refused'; END $$`)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER refuse_restore BEFORE UPDATE ON agent_sandboxes FOR EACH ROW
				WHEN (NEW.org_id = '%s' AND OLD.restore_checkpoint_id IS NULL AND NEW.restore_checkpoint_id IS NOT NULL)
				EXECUTE FUNCTION refuse_restore_write()`, org))
			Expect(err).NotTo(HaveOccurred())
			dropped := false
			drop := func() {
				if !dropped {
					dropped = true
					_, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS refuse_restore ON agent_sandboxes`)
					Expect(err).NotTo(HaveOccurred())
				}
			}
			DeferCleanup(drop)
			cl.Crash(ws, actor, "guest kernel panic")
			_ = r.ReconcileAll(ctx) // the pass reports the failed write
			Expect(cl.Calls("RevertActor")).To(BeZero(), "no revert without the pointer")
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_CRASHED))
			Expect(sandboxRow(ctx, agent.ID).RestoredCheckpointID.Valid).To(BeTrue(), "the mark is intact")

			By("the next pass, with the write working, points the restore and reverts")
			drop()
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeTrue())
		})

		It("sends a crashed actor with a mark and no snapshot to repair, with the restore pointer kept", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			cl.DropSnapshots(ws, actor)
			cl.Crash(ws, actor, "guest kernel panic")
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(BeZero(), "the actor never suspended: a revert would give an empty home")
			Expect(cl.Calls("DeleteActor")).To(Equal(1), "the repair path recreates it")
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeTrue(), "the new actor restores the home: the pointer was written before the revert")
		})

		It("keeps the mark for a stop that it did not send and no newer snapshot is recorded for", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			_, err := d.StopSandbox(ctx, ws, actor) // a stop behind the reconciler's back: its snapshot time is not recorded
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			Expect(sandboxRow(ctx, agent.ID).RestoredCheckpointID.Valid).To(BeTrue())
		})

		It("keeps no mark for a recreated sandbox that got no restore", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			restoreHome(ctx, agent, false)
			restoreHome(ctx, agent, true)
			_, err := pool.Exec(ctx, `UPDATE agent_sandboxes SET sandbox_id = 'another' WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
			Expect(err).NotTo(HaveOccurred())
			cl.Crash(ws, actor, "guest kernel panic")
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeFalse(), "the mark of another sandbox does not count")
		})

		// refuse makes the database refuse an UPDATE or INSERT on table when cond holds (a trigger), until the
		// returned drop runs (also at cleanup).
		refuse := func(ctx SpecContext, table, event, cond string) (drop func()) {
			GinkgoHelper()
			_, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION refuse_test_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test write refused'; END $$`)
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER refuse_test BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION refuse_test_write()`, event, table, cond))
			Expect(err).NotTo(HaveOccurred())
			dropped := false
			drop = func() {
				if !dropped {
					dropped = true
					_, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS refuse_test ON %s`, table))
					Expect(err).NotTo(HaveOccurred())
				}
			}
			DeferCleanup(drop)
			return drop
		}
		// expectHomeKept checks that the restored home is not lost: the restore points at the checkpoint
		// again and the mark stays, also after the passes that would seal an idle sandbox.
		expectHomeKept := func(ctx SpecContext, agent store.Agent, restored pgtype.UUID, why string) {
			GinkgoHelper()
			got := sandboxRow(ctx, agent.ID)
			Expect(got.RestoreCheckpointID).To(Equal(restored), "the next run restores the home: %s", why)
			Expect(got.RestoredCheckpointID.Valid).To(BeTrue(), "the mark stays: %s", why)
		}

		It("keeps the restored home when the revert fails after the restore pointer was written", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			coolDown(ctx, org, agent.ID)
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			cl.Crash(ws, actor, "guest kernel panic")
			cl.FailNextReverts(ws, actor, 1000) // no worker has room, at every try
			advance(0)
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(BeNumerically(">", 0))
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_CRASHED), "the revert did not work")
			expectHomeKept(ctx, agent, restored, "the pointer was written before the revert")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
		})

		It("keeps the restored home when the egress policy write after RevertActor fails", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			coolDown(ctx, org, agent.ID)
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			cl.Crash(ws, actor, "guest kernel panic")
			cl.FailPolicyReadsAfterRevert(ws, actor, 1)
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(1), "the revert worked")
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED), "the actor holds its snapshot")
			Expect(cl.Calls("DeleteActor")).To(BeZero(), "the reverted actor is not deleted")
			expectHomeKept(ctx, agent, restored, "the egress write failed after the revert")

			By("an idle stopped sandbox with the restore pending is not sealed and keeps the mark")
			advance(3 * time.Hour)
			pass(ctx)
			pass(ctx)
			expectHomeKept(ctx, agent, restored, "a stop that this path did not send")
		})

		It("keeps the restored home when the audit write after the revert fails", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			coolDown(ctx, org, agent.ID)
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			// audit-lint:allow a trigger makes the database refuse the revert's audit write, to prove the home survives it
			drop := refuse(ctx, "audit_events", "INSERT", fmt.Sprintf("NEW.org_id = '%s' AND NEW.action = 'sandbox.revert'", org))
			cl.Crash(ws, actor, "guest kernel panic")
			Expect(r.ReconcileAll(ctx)).NotTo(Succeed(), "the pass reports the failed write")
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			expectHomeKept(ctx, agent, restored, "the audit write failed after the revert")

			By("the next passes, with the writes working, do not seal before the restore ran")
			drop()
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			advance(3 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			expectHomeKept(ctx, agent, restored, "the idle stop with the restore pending is not the seal")
		})

		Context("with home checkpoints on", func() {
			BeforeEach(func() {
				checkpoints = true
				r = newR()
			})
			// completeCheckpoint plays agentd's checkpoint writer: a complete checkpoint of the sandbox, taken now.
			completeCheckpoint := func(ctx SpecContext, agent store.Agent) store.HomeCheckpoint {
				GinkgoHelper()
				sb := sandboxRow(ctx, agent.ID)
				_, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String})
				if err != nil {
					Expect(err).To(MatchError(pgx.ErrNoRows), "an open request is the one claimed")
				}
				cp, err := q.ClaimHomeCheckpoint(ctx, store.ClaimHomeCheckpointParams{OrgID: org, LeaseSeconds: 60})
				Expect(err).NotTo(HaveOccurred())
				cp, err = q.CompleteHomeCheckpoint(ctx, store.CompleteHomeCheckpointParams{
					OrgID: org, ID: cp.ID, SnapshotID: pgtype.Text{String: "k" + cp.ID.String(), Valid: true},
					SizeBytes: pgtype.Int8{Int64: 1, Valid: true}, Generation: sb.HomeGeneration.String,
				})
				Expect(err).NotTo(HaveOccurred())
				return cp
			}
			// dirtyHome stands in for a run that wrote the home after the checkpoint.
			dirtyHome := func(ctx SpecContext, agent store.Agent) {
				GinkgoHelper()
				_, err := pool.Exec(ctx, `UPDATE agent_sandboxes SET home_dirtied_at = now() WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
				Expect(err).NotTo(HaveOccurred())
			}
			// firstBoot creates the agent's actor and returns it once the first-boot seal is recorded.
			firstBoot := func(ctx SpecContext) (store.Agent, string) {
				GinkgoHelper()
				agent := newAgent(ctx, org, user)
				pass(ctx)
				actor := sandboxRow(ctx, agent.ID).ActorName.String
				Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
				pass(ctx) // the row says ready: the checkpoint writer reads only a ready sandbox
				Expect(cl.Calls("SuspendActor")).To(Equal(1), "the seal of the first boot")
				Expect(sandboxRow(ctx, agent.ID).LastSnapshotAt.Valid).To(BeTrue(), "the seal's time is recorded")
				return agent, actor
			}

			It("points the restore at a stale checkpoint that is newer than the first-boot seal, after a leader change", func(ctx SpecContext) {
				agent, actor := firstBoot(ctx)
				cp := completeCheckpoint(ctx, agent)
				dirtyHome(ctx, agent) // the checkpoint is stale: the home was written after it
				Expect(cp.StartedAt.Time).To(BeTemporally(">", sandboxRow(ctx, agent.ID).LastSnapshotAt.Time), "one clock: the database's")
				r = newR() // only the Postgres state: the snapshot time comes from the row
				cl.Crash(ws, actor, "guest kernel panic")
				pass(ctx)
				Expect(cl.Calls("RevertActor")).To(BeZero(), "a revert would give the empty home of the first boot")
				Expect(cl.Calls("DeleteActor")).To(Equal(1), "the repair path recreates the sandbox")
				Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID).To(Equal(cp.ID), "the new sandbox restores the checkpoint")
			})

			It("points the restore at the checkpoint when the snapshot time is unknown", func(ctx SpecContext) {
				agent, actor := firstBoot(ctx)
				cp := completeCheckpoint(ctx, agent)
				_, err := pool.Exec(ctx, `UPDATE agent_sandboxes SET last_snapshot_at = NULL WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
				Expect(err).NotTo(HaveOccurred())
				cl.Crash(ws, actor, "guest kernel panic")
				pass(ctx)
				Expect(cl.Calls("RevertActor")).To(BeZero(), "an unknown snapshot time loses to the checkpoint")
				Expect(cl.Calls("DeleteActor")).To(Equal(1))
				Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID).To(Equal(cp.ID))
			})

			It("reverts when the checkpoint started before the snapshot and finished after it", func(ctx SpecContext) {
				agent, actor := firstBoot(ctx)
				cp := completeCheckpoint(ctx, agent)
				// The checkpoint read the home before the snapshot was taken: its content is the older one.
				_, err := pool.Exec(ctx, `UPDATE home_checkpoints SET started_at = s.last_snapshot_at - interval '1 second', finished_at = s.last_snapshot_at + interval '1 minute'
					FROM agent_sandboxes s WHERE home_checkpoints.org_id = $1 AND home_checkpoints.id = $2 AND s.org_id = $1 AND s.agent_id = $3`, org, cp.ID, agent.ID)
				Expect(err).NotTo(HaveOccurred())
				cl.Crash(ws, actor, "guest kernel panic")
				pass(ctx)
				Expect(cl.Calls("RevertActor")).To(Equal(1), "the start time of the checkpoint counts, not its end")
				Expect(cl.Calls("DeleteActor")).To(BeZero())
			})

			It("reverts when the snapshot is newer than the checkpoint", func(ctx SpecContext) {
				agent, actor := firstBoot(ctx)
				completeCheckpoint(ctx, agent)
				_, err := pool.Exec(ctx, `UPDATE agent_sandboxes SET last_snapshot_at = now() + interval '1 minute' WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
				Expect(err).NotTo(HaveOccurred())
				cl.Crash(ws, actor, "guest kernel panic")
				pass(ctx)
				Expect(cl.Calls("RevertActor")).To(Equal(1))
				Expect(cl.Calls("DeleteActor")).To(BeZero())
				Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeFalse())
			})
		})

		It("after a leader change, a new reconciler with only the Postgres state neither clears the seal mark nor reverts without the restore", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			coolDown(ctx, org, agent.ID)
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			cl.Crash(ws, actor, "guest kernel panic")
			cl.FailNextReverts(ws, actor, 5) // every try of one pass: RevertActor and its retries
			pass(ctx)
			Expect(cl.State(ws, actor)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_CRASHED))
			expectHomeKept(ctx, agent, restored, "the old leader wrote the pointer before its revert failed")

			By("the new leader has no memory of the restore, the failed revert or its backoff")
			r = newR()
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(6), "the new leader reverts")
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			expectHomeKept(ctx, agent, restored, "the pointer is in Postgres")

			By("an idle sandbox with the restore pending is not sealed: the reverted home lacks the restore")
			advance(3 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			expectHomeKept(ctx, agent, restored, "the idle stop with the restore pending is not the seal")
		})

		It("stops a front that fails the wakeup probe at every resume, and never deletes the reverted actor", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			cl.FailProbe(ws, actor, 100)
			cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
			for range 5 {
				pass(ctx)
			}
			Expect(cl.Calls("RevertActor")).To(Equal(3), "the cap counts a revert whose resume failed")
			Expect(cl.Calls("DeleteActor")).To(BeZero(), "the repair path never deletes the reverted actor")
			a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(a.UnhealthyAt.Valid).To(BeTrue())
			Expect(a.UnhealthyReason.String).To(ContainSubstring("the front kept failing"))
		})

		It("reverts again after an admin resumed the agent: the cap counts from the resume", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			for range 3 {
				Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
				cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
				pass(ctx)
			}
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(3), "the cap is reached")
			time.Sleep(20 * time.Millisecond) // the database clock moves past the last revert
			_, err := q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(4), "the reverts before the resume no longer count")
		})

		It("backs off a revert that fails before RevertActor worked, and gives the sandbox to repair after enough tries", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			cl.Crash(ws, actor, "guest kernel panic")
			cl.FailNextReverts(ws, actor, 1000) // no worker has room, at every try
			advance(0)
			pass(ctx)
			first := cl.Calls("RevertActor")
			Expect(first).To(BeNumerically(">", 0))
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(first), "inside the backoff no new try is sent")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			for i := range 8 {
				advance(time.Duration(i+1) * 10 * time.Minute)
				pass(ctx)
			}
			Expect(cl.Calls("DeleteActor")).To(Equal(1), "after the tries the repair path takes the sandbox")
		})

		It("points the restore at the restored home before the repair path takes a sandbox whose reverts kept failing", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			cl.Crash(ws, actor, "guest kernel panic")
			cl.FailNextReverts(ws, actor, 1000) // no worker has room, at every try
			advance(0)
			pass(ctx) // the first try
			// tries 2 to 6, each after its backoff
			for i := range 5 {
				advance(time.Duration(i+1) * 10 * time.Minute)
				pass(ctx)
			}
			Expect(cl.Calls("DeleteActor")).To(BeZero(), "the tries are not over yet")
			// A restore ended meanwhile: the mark is set and no restore is pending.
			restoreHome(ctx, agent, false)
			restored := sandboxRow(ctx, agent.ID).RestoreCheckpointID
			restoreHome(ctx, agent, true)
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID.Valid).To(BeFalse())
			advance(time.Hour)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(Equal(1), "after the tries the repair path takes the sandbox")
			Expect(sandboxRow(ctx, agent.ID).RestoreCheckpointID).To(Equal(restored), "the new sandbox restores the home: the pointer was written before the repair")
		})

		It("stops at the retry cap, marks the agent unhealthy and reverts no more", func(ctx SpecContext) {
			agent := newAgent(ctx, org, user)
			pass(ctx)
			actor := sandboxRow(ctx, agent.ID).ActorName.String
			for i := range 3 {
				Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING), "revert %d", i)
				cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
				pass(ctx)
			}
			Eventually(running(actor)).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(cl.Calls("RevertActor")).To(Equal(3))
			cl.Crash(ws, actor, "wakeup probe: GET /readyz: connection refused")
			pass(ctx)
			Expect(cl.Calls("RevertActor")).To(Equal(3), "the cap is reached")
			a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(a.UnhealthyAt.Valid).To(BeTrue())
			Expect(a.UnhealthyReason.String).To(ContainSubstring("the front kept failing"))
		})
	})

	// Issue #1489: the model listener checks the actor JWT against the actor table of its own copy, so the
	// run waits for the model copies to ack the actor revision, not only the MCP copies.
	It("holds the forge row until every live model gateway copy acked the actor revision", func(ctx SpecContext) {
		// A copy that serves only the model half: it never acks the MCP half.
		Expect(staterev.TouchCopy(ctx, pool, "model-copy-1", "test-stream", false)).To(Succeed())
		DeferCleanup(func() { _ = staterev.ForgetCopy(context.Background(), pool, "model-copy-1", "test-stream") })
		agent := newAgent(ctx, org, user)
		pass(ctx)
		pass(ctx)
		row, err := q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.StateRevision.Valid).To(BeTrue(), "the row waits for the revision of the actor")
		Expect(row.ServedAt.Valid).To(BeFalse(), "no model copy acked yet")
		var first int64
		Expect(pool.QueryRow(ctx, "SELECT min(revision) FROM gateway_state_revisions").Scan(&first)).To(Succeed())
		Expect(first).To(BeNumerically("<", row.StateRevision.Int64), "an earlier revision exists")

		By("a live model copy that holds an older revision keeps the row unserved")
		Expect(staterev.Ack(ctx, pool, "model-copy-1", first, gwv1.Half_HALF_MODEL)).To(Succeed())
		pass(ctx)
		row, err = q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ServedAt.Valid).To(BeFalse())

		By("the row is served when the model copy acks the actor revision")
		Expect(staterev.Ack(ctx, pool, "model-copy-1", row.StateRevision.Int64, gwv1.Half_HALF_MODEL)).To(Succeed())
		pass(ctx)
		row, err = q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.ServedAt.Valid).To(BeTrue())
	})

	// Issue #1386: an actor made before actors had their own names runs under the sandbox name and has no
	// UID, so it is never in the State and the gateway refuses its tokens. The reconciler recreates it.
	It("recreates an actor that runs under its sandbox name, and names the new one", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		first := sandboxRow(ctx, agent.ID)
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, first.ActorName.String) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))

		By("the legacy shape: the actor runs under the sandbox name, with the same template, and no actor name is recorded")
		cur, err := ctl.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: ws, Name: first.ActorName.String}})
		Expect(err).NotTo(HaveOccurred())
		legacy := first.SandboxName
		_, err = ctl.CreateActor(ctx, &ateapiv1.CreateActorRequest{Actor: &ateapiv1.Actor{
			Metadata: &ateapiv1.ResourceMetadata{Atespace: ws, Name: legacy}, ActorTemplate: cur.GetActorTemplate(),
		}})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctl.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: ws, Name: first.ActorName.String}})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE agent_sandboxes SET actor_name = NULL, actor_uid = NULL WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(sandboxRow(ctx, agent.ID).ActorRef()).To(Equal(legacy))
		Expect(cl.State(ws, legacy)).NotTo(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "the legacy actor exists")

		Eventually(func(g Gomega) {
			g.Expect(r.ReconcileAll(ctx)).To(Succeed())
			sb := sandboxRow(ctx, agent.ID)
			g.Expect(sb.ActorName.Valid).To(BeTrue(), "a new actor name is recorded")
			g.Expect(sb.ActorUid.Valid).To(BeTrue())
		}).Should(Succeed())
		next := sandboxRow(ctx, agent.ID)
		Expect(next.ActorName.String).NotTo(Equal(legacy))
		Expect(cl.State(ws, legacy)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "the legacy actor is gone")
		pass(ctx)
		Expect(stateActors(ctx, agent.ID)).To(HaveLen(1), "the new actor is in the State")
	})

	// Issue #1386, ADR 0120 amendment item 5: the State carries only the actor of a sandbox that is not
	// deleted. A token that atenet-egress minted for a deleted actor must not identify the agent.
	It("takes the actor of a deleted task sandbox out of the gateway State", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
		queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		run := claimRun(ctx, org)
		pass(ctx)
		task := sandboxRow(ctx, agent.ID)
		Expect(task.ActorUid.Valid).To(BeTrue())
		pass(ctx)
		Expect(stateActors(ctx, agent.ID)).To(HaveLen(1), "the task actor is in the State")

		_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateSucceeded})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: org, ID: run.TaskID, State: store.TaskStateCompleted})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		pass(ctx)
		Expect(cl.State(ws, task.ActorName.String)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "the task actor is gone")
		Expect(stateActors(ctx, agent.ID)).To(BeEmpty(), "no State entry for the deleted actor")
	})

	// Issue #1386: a create can make an actor that goes before a pass records its UID. atenet-egress keeps
	// tokens by actor name, so the next create must not use that name again. The driver forgets the
	// gateway access of the old name (ForgetGatewayAccess, tested with the driver).
	It("never sends an actor name to a create twice, also when no UID was recorded", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		first := sandboxRow(ctx, agent.ID)
		Expect(first.ActorName.Valid).To(BeTrue())
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, first.ActorName.String) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))

		By("the actor goes behind the reconciler's back, and its UID is not recorded")
		_, err := pool.Exec(ctx, `UPDATE agent_sandboxes SET actor_uid = NULL WHERE org_id = $1 AND agent_id = $2`, org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = ctl.DeleteActor(ctx, &ateapiv1.DeleteActorRequest{Actor: &ateapiv1.ObjectRef{Atespace: ws, Name: first.ActorName.String}})
		Expect(err).NotTo(HaveOccurred())
		creates, deletes := cl.Calls("CreateActor"), cl.Calls("DeleteActor")

		pass(ctx)
		second := sandboxRow(ctx, agent.ID)
		Expect(second.ActorName.String).NotTo(Equal(first.ActorName.String), "a new name for the new actor")
		Expect(second.ActorName.String).To(HavePrefix(first.SandboxName + "-"))
		Expect(cl.Calls("CreateActor")).To(Equal(creates + 1))
		Expect(cl.Calls("DeleteActor")).To(Equal(deletes), "the old name holds no actor: only the driver's record of it goes")
		Expect(cl.State(ws, first.ActorName.String)).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "nothing under the old name")
		Eventually(func() ateapiv1.ActorState { return cl.State(ws, second.ActorName.String) }).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
		Expect(cl.EgressAllowed(ws, second.ActorName.String, fakeate.GatewayHost, fakeate.GatewayPort)).To(BeTrue(), "the new name has its gateway access")
	})
})

// The launcher applies one fixed policy to every guest process (D1); the reconciler's SandboxPolicy
// is what OpenShell enforces per sandbox. The two cover the same paths, so the fixed policy cannot
// get narrower or wider than the per-sandbox one unseen (issue #1245). The guest runs the policy
// file of its tier ([harness.LauncherPrefix]), which replaces the lists of the default policy as a
// whole, so the files are compared too.
var _ = Describe("The launcher policy and the per-sandbox filesystem policy", func() {
	load := func(name string) guestlaunch.LandlockPolicy {
		f, err := os.Open("../../cmd/koinon-guest-launcher/" + name)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(f.Close)
		p, err := guestlaunch.ParsePolicy(f)
		Expect(err).NotTo(HaveOccurred())
		return p.Landlock
	}
	DescribeTable("cover the same paths with the same access",
		func(get func() guestlaunch.LandlockPolicy) {
			lp := get()
			fs := reconciler.SandboxPolicy(nil).Filesystem
			// Paths that only the launcher names: the guest is a full Linux root, not an OpenShell
			// supervisor view. Each needs a reason; a new entry here is a decision.
			launcherOnly := map[string]string{
				"/sbin": "same tree as /bin on the guest images", "/lib32": "same tree as /lib",
				"/sys": "read by runtimes for cpu and memory limits", "/run": "runtime sockets and pid files",
				"/var/lib": "package and tool state, read only", "/var/tmp": "scratch, like /tmp",
				"/home": "agent home", "/root": "agent home when the tier runs as root", "/var/cache": "tool caches",
				"/sandbox": "the working directory (IncludeWorkdir)",
			}
			// OpenShell names two device nodes; the launcher lists every device it allows, one by one.
			perSandboxDevices := map[string]bool{"/dev/urandom": true, "/dev/null": true}
			launcher := map[string]bool{}
			for _, p := range slices.Concat(lp.ReadExec, lp.ReadWrite, lp.Devices) {
				launcher[p] = true
			}
			sandbox := map[string]bool{"/sandbox": fs.IncludeWorkdir}
			for _, p := range slices.Concat(fs.ReadOnly, fs.ReadWrite) {
				sandbox[p] = true
			}
			for p := range sandbox {
				if perSandboxDevices[p] {
					Expect(lp.Devices).To(ContainElement(p), "device %s must be in the launcher's device list", p)
					continue
				}
				Expect(launcher).To(HaveKey(p), "%s is in the per-sandbox policy but not in the launcher policy", p)
			}
			for p := range launcher {
				if _, ok := launcherOnly[p]; ok || perSandboxDevices[p] || strings.HasPrefix(p, "/dev/") {
					continue
				}
				Expect(sandbox).To(HaveKey(p), "%s is in the launcher policy but not in the per-sandbox policy: add it there or to launcherOnly with a reason", p)
			}
			// Access, both ways. Writable per sandbox must be writable in the launcher (a narrower
			// policy breaks the agent); read-only per sandbox must not be writable there, but for the
			// known difference: the launcher lets the guest write /var/log. Listing it keeps the
			// difference visible: a new one fails, and so does the removal of this one.
			writable := slices.Concat(lp.ReadWrite, lp.Devices)
			rw := slices.Clone(fs.ReadWrite)
			if fs.IncludeWorkdir {
				rw = append(rw, "/sandbox")
			}
			for _, p := range rw {
				Expect(writable).To(ContainElement(p), "%s is read-write per sandbox but not writable in the launcher policy", p)
			}
			widened := []string{}
			for _, p := range fs.ReadOnly {
				if slices.Contains(lp.ReadWrite, p) {
					widened = append(widened, p)
				}
			}
			Expect(widened).To(Equal([]string{"/var/log"}), "paths read-only per sandbox but writable in the launcher")
		},
		Entry("default policy", func() guestlaunch.LandlockPolicy { return guestlaunch.DefaultPolicy().Landlock }),
		Entry("policy-vm.json", func() guestlaunch.LandlockPolicy { return load("policy-vm.json") }),
		Entry("policy-gvisor.json", func() guestlaunch.LandlockPolicy { return load("policy-gvisor.json") }),
	)
})
