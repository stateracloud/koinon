// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
		now  atomic.Pointer[time.Time]
		ws   string
		ctl  ateapiv1.ControlClient
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
		d = substrate.New(control, router, substrate.Config{
			PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
			GatewayRoutes: []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)},
			CodeHostEgressRoute: func() *substrate.GatewayRoute {
				r := substrate.EgressRoute(fakeate.GatewayHost, fakeate.EgressPort)
				return &r
			}(),
			GuestSigner: fakeate.Signer(),
			// The class the reconciler requests (reconciler.Config.RuntimeClassName) has a tier.
			RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM},
		})
		now.Store(nil)
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		r, err = reconciler.New(pool, execution.WithoutEgress(d), &fakeModelGateway{},
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
				Logger:         slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Now: func() time.Time {
					if t := now.Load(); t != nil {
						return *t
					}
					return time.Now()
				},
			})
		Expect(err).NotTo(HaveOccurred())
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

		By("a crashed actor is replaced by one with a new name and a new UID")
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
