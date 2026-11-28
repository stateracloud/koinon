// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"archive/tar"
	"bytes"
	"context"
	"log/slog"
	"path"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// substrateImage2 is a second pinned image: a new harness image changes the sandbox spec.
const substrateImage2 = "registry.example.com/koinon/sandbox@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// The agent home through the sandbox lifecycle on the Substrate driver (issue #1090, slice S1).
//
// These specs record the behaviour on main as it is; they choose no home model. Facts they rest on:
// the Substrate driver mounts one durable directory at the home (substrate.HomeMountPath, slice S5),
// and a suspend keeps it with the actor's memory and root filesystem snapshot. The only
// copy outside Substrate is the kopia home checkpoint, which is off by default
// (reconciler.Config.Checkpoints). The fake Substrate keeps an actor's files across a suspend and
// drops them with DeleteActor, as Substrate does with the snapshot prefix (lab, 2026-11-24).
//
// Each spec that shows a loss of the home says so in a "HOME LOST" comment.
var _ = Describe("The agent home on the Substrate driver", func() {
	const marker = "notes/marker.txt"
	markerPath := path.Join(checkpoint.HomeDir, marker)
	markerBody := []byte("written by the agent before the idle stop\n")

	var (
		cl    *fakeate.Cluster
		kube  *fake.Clientset
		d     *substrate.Driver
		cfg   reconciler.Config
		r     *reconciler.Reconciler
		org   pgtype.UUID
		user  pgtype.UUID
		agent store.Agent
		ws    string
		now   atomic.Pointer[time.Time]
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	at := func(off time.Duration) {
		t := time.Now().Add(off)
		now.Store(&t)
	}
	build := func() {
		GinkgoHelper()
		var err error
		r, err = reconciler.New(pool, execution.WithoutEgress(d), &fakeModelGateway{},
			reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
	}
	row := func(ctx context.Context) store.AgentSandbox {
		GinkgoHelper()
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb
	}
	// actor is the name of the agent's current Substrate actor: each new actor gets a new one (issue #1386).
	actor := func() string { return row(context.Background()).ActorRef() }
	state := func() ateapiv1.ActorState { return cl.State(ws, actor()) }
	running := func() {
		GinkgoHelper()
		Eventually(state).Should(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
	}
	// home is the marker file in the actor's guest, or nil when the actor or the file is not there.
	home := func() []byte {
		b, ok := cl.File(ws, actor(), markerPath)
		if !ok {
			return nil
		}
		return b
	}
	// writeMarker plays the agent: it writes a file into its home through the guest's file service,
	// the path every upload into a Substrate guest takes.
	writeMarker := func(ctx context.Context) {
		GinkgoHelper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		Expect(tw.WriteHeader(&tar.Header{Name: marker, Mode: 0o600, Size: int64(len(markerBody))})).To(Succeed())
		_, err := tw.Write(markerBody)
		Expect(err).NotTo(HaveOccurred())
		Expect(tw.Close()).To(Succeed())
		Expect(d.Upload(ctx, ws, actor(), checkpoint.HomeDir, &buf)).To(Succeed())
		Expect(home()).To(Equal(markerBody))
	}
	// idleStop plays a harness run that wrote the home and ended, then moves the clock past the
	// default idle timeout (10 minutes) and runs a pass.
	idleStop := func(ctx context.Context) {
		GinkgoHelper()
		dirtyHome(ctx, org, user, agent.ID)
		coolDown(ctx, org, agent.ID)
		at(11 * time.Minute)
		pass(ctx)
	}

	BeforeEach(func(ctx SpecContext) {
		cl = fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		d = substrate.New(control, router, substrate.Config{
			PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, DrainTimeout: 200 * time.Millisecond,
			GatewayRoutes:     []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)},
			GuestSigner:       fakeate.Signer(),
			RuntimeClassTiers: map[string]harness.Tier{"kata": harness.TierVM},
		})
		kube = fake.NewClientset() // kept across a rebuild of the reconciler, as in a restart
		now.Store(nil)
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		cfg = reconciler.Config{
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: substrateImage, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			RuntimeClassName: "kata",
			// The default is 0 (PR 1250); the agent is keep-warm so that its sandbox exists after the
			// first pass, and coolDown ends that before the idle stop.
			KeepWarmPerOrg: 2,
			Logger:         slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now: func() time.Time {
				if t := now.Load(); t != nil {
					return *t
				}
				return time.Now()
			},
		}
	})

	// start creates the agent with cfg as the spec set it, waits for the running actor and writes
	// the marker into its home.
	start := func(ctx context.Context) {
		GinkgoHelper()
		build()
		agent = newAgent(ctx, org, user)
		pass(ctx)
		running()
		Expect(cl.Calls("CreateActor")).To(Equal(1))
		writeMarker(ctx)
	}

	Context("with home checkpoints off (the default)", func() {
		BeforeEach(func(ctx SpecContext) {
			Expect(cfg.Checkpoints).To(BeFalse())
			start(ctx)
		})

		It("keeps the home through an idle suspend and a resume for a run", func(ctx SpecContext) {
			idleStop(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(row(ctx).State).To(Equal(store.SandboxStateStopped))
			Expect(home()).To(Equal(markerBody), "the suspend snapshot holds the home")

			By("a queued run resumes the same actor, with its home")
			now.Store(nil)
			queueRun(ctx, org, user, agent.ID)
			pass(ctx)
			running()
			Expect(cl.Calls("CreateActor")).To(Equal(1), "resumed, not re-created")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(home()).To(Equal(markerBody))
			Expect(cl.RouterResumes(ws, actor())).To(BeZero(), "the reconciler resumed it, not a guest request")
		})

		It("never archives: the suspended actor, and the home in its snapshot, stay without a time limit", func(ctx SpecContext) {
			idleStop(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

			By("long after the archive window")
			at(reconciler.DefaultArchiveAfter + 24*time.Hour)
			pass(ctx)
			pass(ctx)
			// The archive tier needs checkpoints (archiveDue): with them off, the snapshot is never
			// removed, and the snapshot store holds it for as long as the agent lives.
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(row(ctx).ArchivedAt.Valid).To(BeFalse())
			Expect(home()).To(Equal(markerBody))
		})

		It("loses the home when a new sandbox spec recreates the sandbox", func(ctx SpecContext) {
			By("a new harness image changes the spec")
			cfg.Harnesses[harness.OpenCode] = reconciler.Harness{Image: substrateImage2, Binaries: reconciler.DefaultBinaries[harness.OpenCode]}
			build()
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(Equal(1))
			pass(ctx) // the pass after the deletion creates the new actor
			running()
			// HOME LOST: with checkpoints off, a recreation deletes the actor (and its snapshot prefix)
			// and creates a new one from the image. Nothing restores the home. The marker came in by an
			// upload, not by a run: even with checkpoints on, home_dirtied_at would not protect it.
			Expect(cl.Calls("DeleteActor")).To(Equal(1), "no second delete")
			Expect(cl.Calls("CreateActor")).To(Equal(2))
			Expect(row(ctx).RestoreCheckpointID.Valid).To(BeFalse(), "no checkpoint to restore from")
			Expect(home()).To(BeNil(), "the new actor has a fresh root filesystem")
		})

		It("keeps the actor and the home of the last snapshot when a crashed actor is reverted (issue #1481)", func(ctx SpecContext) {
			idleStop(ctx) // the suspend snapshot holds the marker
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			now.Store(nil)
			queueRun(ctx, org, user, agent.ID)
			pass(ctx)
			running()
			name := actor()
			cl.Crash(ws, name, "guest kernel panic")
			Eventually(func(g Gomega) int {
				g.Expect(r.ReconcileAll(ctx)).To(Succeed())
				return cl.Calls("RevertActor")
			}).Should(Equal(1))
			running()
			Expect(actor()).To(Equal(name))
			Expect(cl.Calls("CreateActor")).To(Equal(1))
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(home()).To(Equal(markerBody))
			Expect(row(ctx).LastSnapshotAt.Valid).To(BeTrue(), "the snapshot time of the idle stop is in Postgres")
		})

		It("loses the writes after the last snapshot when a crashed actor is reverted", func(ctx SpecContext) {
			// HOME LOST (the part after the snapshot): the marker was written after the seal of the first
			// boot, and no checkpoint holds it (checkpoints are off). The revert goes back to the seal.
			cl.Crash(ws, actor(), "guest kernel panic")
			Eventually(func(g Gomega) int {
				g.Expect(r.ReconcileAll(ctx)).To(Succeed())
				return cl.Calls("RevertActor")
			}).Should(Equal(1))
			running()
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(home()).To(BeNil())
		})

		It("loses the home when the repair recreates a crashed actor that has no snapshot", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			cl.DropSnapshots(ws, actor()) // a revert would give an empty home
			cl.Crash(ws, actor(), "guest kernel panic")
			Expect(home()).To(Equal(markerBody), "the crashed actor still has its files")
			Eventually(func(g Gomega) int {
				g.Expect(r.ReconcileAll(ctx)).To(Succeed())
				return cl.Calls("CreateActor")
			}).Should(Equal(2))
			running()
			// HOME LOST: with checkpoints off, the repair path deletes the failed actor (homeGate lets
			// it) and creates a new one. The files of the crashed actor are not read first.
			Expect(cl.Calls("DeleteActor")).To(Equal(1))
			Expect(row(ctx).RestoreCheckpointID.Valid).To(BeFalse(), "no checkpoint to restore from")
			Expect(home()).To(BeNil())
		})

		It("keeps the suspended actor, and so the home, of a deleted agent with retention retain", func(ctx SpecContext) {
			cfg.HomeRetention = reconciler.RetentionRetain
			build()
			idleStop(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(home()).To(Equal(markerBody))
			Expect(row(ctx).State).To(Equal(store.SandboxStateRetained))
		})

		It("deletes the home with the agent", func(ctx SpecContext) {
			idleStop(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			// The default home retention is delete: the actor and its snapshot go, as intended.
			Expect(cl.Calls("DeleteActor")).To(Equal(1))
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED), "the actor is gone")
			Expect(home()).To(BeNil())
			Expect(row(ctx).State).To(Equal(store.SandboxStateDeleted))
		})
	})

	Context("with home checkpoints on", func() {
		// finish plays koinon-agentd's checkpoint writer: it takes the open request and completes or
		// fails it. agentd reads the home with tar over exec; the fake guest has no "tar -c", so no
		// bytes go to a kopia store here (see the restore note below).
		finish := func(ctx context.Context, ok bool) store.HomeCheckpoint {
			GinkgoHelper()
			cp, err := q.ClaimHomeCheckpoint(ctx, store.ClaimHomeCheckpointParams{OrgID: org, LeaseSeconds: 60})
			Expect(err).NotTo(HaveOccurred())
			if ok {
				cp, err = q.CompleteHomeCheckpoint(ctx, store.CompleteHomeCheckpointParams{
					OrgID: org, ID: cp.ID, SnapshotID: pgtype.Text{String: "k" + cp.ID.String(), Valid: true},
					SizeBytes: pgtype.Int8{Int64: int64(len(markerBody)), Valid: true}, Generation: row(ctx).HomeGeneration.String,
				})
			} else {
				cp, err = q.FailHomeCheckpoint(ctx, store.FailHomeCheckpointParams{
					OrgID: org, ID: cp.ID, Error: pgtype.Text{String: "the store said no", Valid: true},
				})
			}
			Expect(err).NotTo(HaveOccurred())
			return cp
		}

		BeforeEach(func(ctx SpecContext) {
			cfg.Checkpoints = true
			// The idle stop waits on the test clock, which the request's real age does not follow.
			cfg.CheckpointStopWait = 30 * time.Minute
			start(ctx)
		})

		// stopWith runs the idle stop: it asks for a checkpoint first, and suspends once it ends.
		stopWith := func(ctx context.Context, ok bool) store.HomeCheckpoint {
			GinkgoHelper()
			idleStop(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING), "the stop waits for the checkpoint")
			cp := finish(ctx, ok)
			pass(ctx)
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(row(ctx).State).To(Equal(store.SandboxStateStopped))
			Expect(home()).To(Equal(markerBody), "the suspend snapshot holds the home")
			return cp
		}

		It("archives after the stopped window: DeleteActor drops the home, and only the checkpoint holds it", func(ctx SpecContext) {
			cp := stopWith(ctx, true)
			lineage := row(ctx).HomeGeneration.String

			By("not before the window")
			at(reconciler.DefaultArchiveAfter - 24*time.Hour)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(home()).To(Equal(markerBody))

			By("after it, the archive deletes the actor and its snapshot")
			at(reconciler.DefaultArchiveAfter + 24*time.Hour)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(Equal(1))
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_UNSPECIFIED))
			Expect(home()).To(BeNil(), "the snapshot prefix is empty after DeleteActor")
			got := row(ctx)
			Expect(got.ArchivedAt.Valid).To(BeTrue())
			Expect(got.RestoreCheckpointID).To(Equal(cp.ID), "the stop checkpoint is now the only copy of the home")

			By("a queued run creates a new actor with an empty home that owes the restore")
			now.Store(nil)
			queueRun(ctx, org, user, agent.ID)
			pass(ctx)
			running()
			Expect(cl.Calls("CreateActor")).To(Equal(2))
			// The restore itself is koinon-agentd's (restoreHome), in the first run, before the
			// harness starts; it is not part of the reconciler. Until it runs, the new actor's home
			// is the image's: the marker is not there.
			Expect(home()).To(BeNil())
			got = row(ctx)
			Expect(got.ArchivedAt.Valid).To(BeFalse())
			Expect(got.RestoreCheckpointID).To(Equal(cp.ID))
			Expect(got.HomeGeneration.String).To(Equal(lineage))
		})

		It("does not archive when the stop checkpoint failed: the home stays only in the suspend snapshot", func(ctx SpecContext) {
			stopWith(ctx, false)
			at(reconciler.DefaultArchiveAfter + 24*time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
			Expect(state()).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(row(ctx).ArchivedAt.Valid).To(BeFalse())
			Expect(home()).To(Equal(markerBody))
		})

		It("deletes the home with the agent; the checkpoint records stay", func(ctx SpecContext) {
			cp := stopWith(ctx, true)
			_, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			Expect(cl.Calls("DeleteActor")).To(Equal(1))
			Expect(home()).To(BeNil())
			Expect(row(ctx).State).To(Equal(store.SandboxStateDeleted))
			kept, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: org, ID: cp.ID})
			Expect(err).NotTo(HaveOccurred(), "the reconciler does not delete checkpoints with the sandbox")
			Expect(kept.Status).To(Equal(store.CheckpointStatusComplete))
		})
	})
})
