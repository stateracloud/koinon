// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// The reconciler's side of home checkpoints (plan item HC1): it asks koinon-agentd for them,
// waits, and deletes a sandbox only after a checkpoint taken since its last activity is complete.
var _ = Describe("Reconciler home checkpoints", func() {
	var (
		osg     *fakegateway.Gateway
		backend *openshell.Backend
		cfg     reconciler.Config
		r       *reconciler.Reconciler
		org     pgtype.UUID
		user    pgtype.UUID
		agent   store.Agent
		names   execution.AgentNames
		now     time.Time
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	row := func(ctx context.Context) store.AgentSandbox {
		GinkgoHelper()
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb
	}
	open := func(ctx context.Context) store.HomeCheckpoint {
		GinkgoHelper()
		cp, err := q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return cp
	}
	// finish plays koinon-agentd's writer: it takes the open request and completes or fails it.
	finish := func(ctx context.Context, ok bool) store.HomeCheckpoint {
		GinkgoHelper()
		cp, err := q.ClaimHomeCheckpoint(ctx, store.ClaimHomeCheckpointParams{OrgID: org, LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		if ok {
			cp, err = q.CompleteHomeCheckpoint(ctx, store.CompleteHomeCheckpointParams{
				OrgID: org, ID: cp.ID, SnapshotID: pgtype.Text{String: "k" + cp.ID.String(), Valid: true},
				SizeBytes: pgtype.Int8{Int64: 10, Valid: true}, Generation: row(ctx).HomeGeneration.String,
			})
		} else {
			cp, err = q.FailHomeCheckpoint(ctx, store.FailHomeCheckpointParams{
				OrgID: org, ID: cp.ID, Error: pgtype.Text{String: "S3 said no", Valid: true},
			})
		}
		Expect(err).NotTo(HaveOccurred())
		return cp
	}
	bump := func() {
		cfg.Flavors[store.AgentFlavorOpencode] = reconciler.Flavor{
			Image: image + "-2", Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode],
		}
		var err error
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		backend = openshell.New(conn)
		now = time.Now()
		cfg = reconciler.Config{
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Flavors: map[store.AgentFlavor]reconciler.Flavor{
				store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			},
			RuntimeClassName: "kata",
			Logger:           slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
			Now:              func() time.Time { return now },
			Checkpoints:      true,
			// The idle stop needs 10 minutes on the test clock, which the request's real age does not follow.
			CheckpointStopWait: 30 * time.Minute,
		}
		org, user = newOrg(ctx)
		addProvider(ctx, org, user, "upstream", "", "fake")
		agent = newAgent(ctx, org, user)
		names = execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(row(ctx).State).To(Equal(store.SandboxStateReady))
	})

	sandboxID := func() string {
		return osg.Sandbox(row(context.Background()).Workspace, names.Sandbox).GetMetadata().GetId()
	}

	It("keeps the sandbox until its home is backed up, then recreates it with the same home generation", func(ctx SpecContext) {
		lineage, oldID := row(ctx).HomeGeneration.String, sandboxID()
		Expect(lineage).To(Equal(oldID))
		dirtyHome(ctx, org, user, agent.ID)
		bump()

		By("a changed spec asks for a backup and deletes nothing")
		pass(ctx)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		cp := open(ctx)
		Expect(cp.Reason).To(Equal(store.CheckpointReasonRecreate))
		Expect(cp.SandboxID).To(Equal(oldID))
		Expect(row(ctx).Message).To(Equal("waiting for the home backup before recreating the sandbox"))

		By("the finished backup lets it go, and the new sandbox keeps the generation")
		done := finish(ctx, true)
		pass(ctx) // deletes
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
		Expect(row(ctx).RestoreCheckpointID).To(Equal(pgtype.UUID{Bytes: done.ID.Bytes, Valid: true}))
		pass(ctx) // creates
		Expect(sandboxID()).NotTo(Equal(oldID))
		got := row(ctx)
		Expect(got.State).To(Equal(store.SandboxStateReady))
		Expect(got.HomeGeneration.String).To(Equal(lineage), "a restored home keeps its generation (ADR 0051)")
		Expect(got.SandboxID.String).To(Equal(sandboxID()))

		By("the generation stays through the restore and later passes")
		Expect(q.ClearSandboxRestore(ctx, store.ClearSandboxRestoreParams{OrgID: org, AgentID: agent.ID})).To(Succeed())
		pass(ctx)
		Expect(row(ctx).HomeGeneration.String).To(Equal(lineage))
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
	})

	It("starts at most MaxRecreatesPerPass home checkpoints per pass after a supervisor change", func(ctx SpecContext) {
		sup := &fakeSupervisor{image: "ghcr.io/nvidia/openshell/supervisor:0.1.5"}
		cfg.Supervisor = sup
		cfg.MaxRecreatesPerPass = 1
		var err error
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())

		// The BeforeEach sandbox was made without the label, so it counts as changed once the image is
		// known and is recreated (clean home, no checkpoint). Let it converge first, then give it a
		// dirty home too: otherwise it takes the pass's one slot below and is deleted without a
		// checkpoint, and nothing tests the cap in front of the gate.
		pass(ctx) // deletes
		pass(ctx) // creates, with the label
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
		Expect(row(ctx).State).To(Equal(store.SandboxStateReady))
		dirtyHome(ctx, org, user, agent.ID)

		// Agents created now get sandboxes that carry the supervisor label. Their homes are dirtied
		// only once the sandboxes exist: home_dirtied_at belongs to the current sandbox, so a run
		// before the sandbox leaves the home looking untouched and the gate lets it go unbacked.
		orgs := []pgtype.UUID{org}
		type owner struct{ org, user, agent pgtype.UUID }
		var more []owner
		for range 3 {
			o, u := newOrg(ctx)
			addProvider(ctx, o, u, "upstream", "", "fake")
			a := newAgent(ctx, o, u)
			more = append(more, owner{o, u, a.ID})
			orgs = append(orgs, o)
		}
		pass(ctx)
		pass(ctx)
		for _, m := range more {
			dirtyHome(ctx, m.org, m.user, m.agent)
		}

		sup.image = "ghcr.io/nvidia/openshell/supervisor:0.1.6"
		pass(ctx)
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM home_checkpoints WHERE org_id = ANY($1) AND reason = 'recreate'`, orgs).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1), "the pass's one slot is taken before the gate asks for a checkpoint")
	})

	It("a sandbox recreated without a checkpoint gets a new generation", func(ctx SpecContext) {
		cfg.Checkpoints = false
		old := row(ctx).HomeGeneration.String
		bump()
		pass(ctx)
		pass(ctx)
		Expect(row(ctx).HomeGeneration.String).NotTo(Equal(old))
	})

	It("blocks the recreation when the backup fails, says why on the agent, and tries again after a resume", func(ctx SpecContext) {
		oldID := sandboxID()
		dirtyHome(ctx, org, user, agent.ID)
		bump()
		pass(ctx)
		finish(ctx, false)

		pass(ctx)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		Expect(sandboxID()).To(Equal(oldID))
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.UnhealthyAt.Valid).To(BeTrue())
		Expect(a.UnhealthyReason.String).To(And(ContainSubstring("home backup failed"), ContainSubstring("S3 said no")))
		Expect(row(ctx).Message).To(ContainSubstring("the home backup failed"))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.mark_unhealthy'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1), "marked once")

		By("an admin resumes the agent: a new backup is requested")
		_, err = q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: org, ID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(open(ctx).Status).To(Equal(store.CheckpointStatusRequested))
		finish(ctx, true)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
	})

	It("does not block on a recreate checkpoint that checkpoints resync marked failed for a missing snapshot: it backs the home up again (#263)", func(ctx SpecContext) {
		oldID := sandboxID()
		dirtyHome(ctx, org, user, agent.ID)
		bump()
		pass(ctx)
		cp := finish(ctx, true)
		// Before the next pass, a database restore and a resync: the snapshot is gone from the bucket.
		n, err := q.MarkHomeCheckpointSnapshotMissing(ctx, store.MarkHomeCheckpointSnapshotMissingParams{
			OrgID: org, ID: cp.ID, SnapshotID: cp.SnapshotID.String,
			Error: pgtype.Text{String: "checkpoints resync: snapshot " + cp.SnapshotID.String + " is no longer in the bucket (repository epoch 0)", Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(1))

		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(0), "no backup of the home is complete")
		Expect(sandboxID()).To(Equal(oldID))
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.UnhealthyAt.Valid).To(BeFalse(), "not blocked as after a failed backup")
		Expect(row(ctx).Message).NotTo(ContainSubstring("the home backup failed"))
		next := open(ctx)
		Expect(next.ID).NotTo(Equal(cp.ID), "a new backup is asked for")
		Expect(next.Reason).To(Equal(store.CheckpointReasonRecreate))

		finish(ctx, true)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1), "recreated once the new backup is complete")
	})

	It("recreates a sandbox found stuck only after its backup, and the new one keeps the generation", func(ctx SpecContext) {
		lineage, oldID := row(ctx).HomeGeneration.String, sandboxID()
		dirtyHome(ctx, org, user, agent.ID)
		_, err := q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{OrgID: org, AgentID: agent.ID, Reason: "a process could not be ended", Generation: oldID})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		Expect(open(ctx).Reason).To(Equal(store.CheckpointReasonRecreate))
		finish(ctx, true)
		pass(ctx)
		pass(ctx)
		Expect(sandboxID()).NotTo(Equal(oldID))
		got := row(ctx)
		Expect(got.HomeGeneration.String).To(Equal(lineage))
		Expect(got.UnhealthyAt.Valid).To(BeFalse(), "the mark ended with the sandbox it was made for")
	})

	It("does not ask again while the previous recreation's restore is pending", func(ctx SpecContext) {
		dirtyHome(ctx, org, user, agent.ID)
		bump()
		pass(ctx)
		finish(ctx, true)
		pass(ctx)
		pass(ctx)
		Expect(row(ctx).RestoreCheckpointID.Valid).To(BeTrue())
		cfg.Flavors[store.AgentFlavorOpencode] = reconciler.Flavor{Image: image + "-3", Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]}
		var err error
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(2), "the home is still in the checkpoint; the empty sandbox holds nothing")
		_, err = q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
		Expect(err).To(HaveOccurred())
	})

	Describe("a sandbox that cannot be read (phase Error)", func() {
		fail := func() {
			GinkgoHelper()
			sb := row(context.Background())
			osg.SetPhase(sb.Workspace, names.Sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
		}

		It("is not recreated and no unclaimable request is filed when no backup is fresh", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			fail()
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			_, err := q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
			Expect(err).To(HaveOccurred(), "agentd reads only a ready sandbox: a request would never be claimed")
			a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(a.UnhealthyAt.Valid).To(BeTrue())
			Expect(a.UnhealthyReason.String).To(ContainSubstring("cannot be read"))
		})

		It("is recreated from the newest complete backup taken since its last activity", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			_, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
				OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sandboxID(), Generation: row(ctx).HomeGeneration.String,
			})
			Expect(err).NotTo(HaveOccurred())
			done := finish(ctx, true)
			fail()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).RestoreCheckpointID).To(Equal(pgtype.UUID{Bytes: done.ID.Bytes, Valid: true}))
		})
	})

	// The gates count from when the home was last written (a harness ran), not from the sandbox's
	// last activity (old issue 329, ADR 0032 amendment): a wake or a start writes nothing.
	Describe("the home-written clock", func() {
		fail := func() {
			GinkgoHelper()
			osg.SetPhase(row(context.Background()).Workspace, names.Sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
		}
		hourly := func(ctx context.Context) store.HomeCheckpoint {
			GinkgoHelper()
			_, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
				OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sandboxID(), Generation: row(ctx).HomeGeneration.String,
			})
			Expect(err).NotTo(HaveOccurred())
			return finish(ctx, true)
		}
		healthy := func(ctx context.Context) bool {
			GinkgoHelper()
			a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			return !a.UnhealthyAt.Valid
		}

		It("recreates an unreadable sandbox from a backup that a later wake and start did not make stale", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			done := hourly(ctx)
			_, err := q.WakeAgentSandbox(ctx, store.WakeAgentSandboxParams{OrgID: org, AgentID: agent.ID})
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE agent_sandboxes SET started_at = now() + interval '1 second' WHERE org_id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			fail()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).RestoreCheckpointID).To(Equal(pgtype.UUID{Bytes: done.ID.Bytes, Valid: true}))
			Expect(healthy(ctx)).To(BeTrue())
		})

		It("recreates an unreadable sandbox no harness ran in, freely and without a restore", func(ctx SpecContext) {
			fail()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).RestoreCheckpointID.Valid).To(BeFalse())
			Expect(healthy(ctx)).To(BeTrue())
			pass(ctx)
			Expect(row(ctx).State).To(Equal(store.SandboxStateReady))
		})

		It("restores a recreated, untouched sandbox from the generation's newest backup", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			Expect(row(ctx).HomeDirtiedAt.Valid).To(BeTrue())
			bump()
			pass(ctx)
			done := finish(ctx, true)
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).HomeDirtiedAt.Valid).To(BeFalse(), "a new sandbox ID starts the clock over")
			Expect(q.ClearSandboxRestore(ctx, store.ClearSandboxRestoreParams{OrgID: org, AgentID: agent.ID})).To(Succeed())

			fail()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(2))
			Expect(row(ctx).RestoreCheckpointID).To(Equal(pgtype.UUID{Bytes: done.ID.Bytes, Valid: true}))
			Expect(healthy(ctx)).To(BeTrue())
		})

		It("still blocks when a harness wrote the home after the last backup", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			hourly(ctx)
			dirtyHome(ctx, org, user, agent.ID)
			fail()
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(healthy(ctx)).To(BeFalse())
		})

		It("does not count a run that failed from starting as a write", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			done := hourly(ctx)
			queueRun(ctx, org, user, agent.ID)
			run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "agentd-test", LeaseSeconds: 60})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.FinishRun(ctx, store.FinishRunParams{
				Outcome: store.RunStateFailed, OutcomeReason: pgtype.Text{String: "sandbox error", Valid: true}, OrgID: org, ID: run.ID, Epoch: run.Epoch,
			})
			Expect(err).NotTo(HaveOccurred())
			fail()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).RestoreCheckpointID).To(Equal(pgtype.UUID{Bytes: done.ID.Bytes, Valid: true}))
		})

		It("stops an idle sandbox no harness ran in without a backup request", func(ctx SpecContext) {
			coolDown(ctx, org, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			Expect(osg.Calls("StopSandbox")).To(Equal(1))
			_, err := q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("the idle stop", func() {
		BeforeEach(func(ctx SpecContext) { coolDown(ctx, org, agent.ID) }) // no keep-warm: idle stops apply
		It("is preceded by a backup", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			Expect(open(ctx).Reason).To(Equal(store.CheckpointReasonStop))
			Expect(osg.Calls("StopSandbox")).To(Equal(0))
			Expect(row(ctx).Message).To(Equal("waiting for the home backup before stopping"))
			finish(ctx, true)
			pass(ctx)
			Expect(osg.Calls("StopSandbox")).To(Equal(1))
		})

		It("goes ahead when the backup failed, because a stop keeps the volume", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			finish(ctx, false)
			pass(ctx)
			Expect(osg.Calls("StopSandbox")).To(Equal(1))
		})

		It("does not wait for ever for a backup that is not taken", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("StopSandbox")).To(Equal(0))
			now = now.Add(cfg.CheckpointStopWait + time.Minute)
			pass(ctx)
			Expect(osg.Calls("StopSandbox")).To(Equal(1))
		})

		It("lets a stopped sandbox be recreated on its stop backup, without a new one", func(ctx SpecContext) {
			dirtyHome(ctx, org, user, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			finish(ctx, true)
			pass(ctx)
			Expect(row(ctx).State).To(Equal(store.SandboxStateStopped))
			bump()
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			_, err := q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
			Expect(err).To(HaveOccurred())
		})
	})

	// The archive tier (plan item AR1, ADR 0032): a long-stopped sandbox with a complete backup is
	// deleted with its volume; the next run gets a fresh sandbox that keeps the home generation.
	Describe("the archive tier", func() {
		BeforeEach(func(ctx SpecContext) { coolDown(ctx, org, agent.ID) }) // no keep-warm: idle stops apply
		stopWith := func(ctx context.Context, ok bool) {
			GinkgoHelper()
			dirtyHome(ctx, org, user, agent.ID)
			now = time.Now().Add(11 * time.Minute)
			pass(ctx)
			finish(ctx, ok)
			pass(ctx)
			Expect(row(ctx).State).To(Equal(store.SandboxStateStopped))
		}
		archivedActions := func(ctx context.Context) int {
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'sandbox.archive'`, org).Scan(&n)).To(Succeed())
			return n
		}

		It("archives after the stopped window, keeps the generation, and creates a sandbox only when a run wants one", func(ctx SpecContext) {
			lineage := row(ctx).HomeGeneration.String
			stopWith(ctx, true)

			By("not before the window")
			now = time.Now().Add(6 * 24 * time.Hour)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))

			By("after it: the sandbox and volume go, the stop backup becomes the restore")
			now = time.Now().Add(8 * 24 * time.Hour)
			creates := osg.Calls("CreateSandbox")
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			got := row(ctx)
			Expect(got.ArchivedAt.Valid).To(BeTrue())
			Expect(got.RestoreCheckpointID.Valid).To(BeTrue())
			Expect(got.Message).To(Equal("archived, restores on next run"))
			Expect(archivedActions(ctx)).To(Equal(1))

			By("nothing recreates it while no run wants it")
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("CreateSandbox")).To(Equal(creates))
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).ArchivedAt.Valid).To(BeTrue())
			Expect(archivedActions(ctx)).To(Equal(1))

			By("a queued run creates a fresh sandbox that keeps the home generation and owes the restore")
			queueRun(ctx, org, user, agent.ID)
			pass(ctx)
			Expect(osg.Calls("CreateSandbox")).To(Equal(creates + 1))
			got = row(ctx)
			Expect(got.ArchivedAt.Valid).To(BeFalse())
			Expect(got.RestoreCheckpointID.Valid).To(BeTrue())
			Expect(got.HomeGeneration.String).To(Equal(lineage), "the same session is reloaded (ADR 0051)")
		})

		It("does not archive when the latest backup failed", func(ctx SpecContext) {
			stopWith(ctx, false)
			now = time.Now().Add(8 * 24 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(row(ctx).ArchivedAt.Valid).To(BeFalse())
			Expect(archivedActions(ctx)).To(Equal(0))
		})

		It("does not archive when the latest backup is older than the sandbox's last activity", func(ctx SpecContext) {
			stopWith(ctx, true)
			_, err := pool.Exec(ctx, `UPDATE home_checkpoints SET finished_at = now() - interval '30 days' WHERE org_id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			now = time.Now().Add(8 * 24 * time.Hour)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		})

		It("does not archive while a newer backup is still requested or running", func(ctx SpecContext) {
			stopWith(ctx, true)
			_, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
				OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sandboxID(), Generation: row(ctx).HomeGeneration.String,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `UPDATE home_checkpoints SET status = 'running' WHERE org_id = $1 AND status = 'requested'`, org)
			Expect(err).NotTo(HaveOccurred())
			now = time.Now().Add(8 * 24 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			Expect(row(ctx).ArchivedAt.Valid).To(BeFalse())
			Expect(archivedActions(ctx)).To(Equal(0))
		})

		It("never archives an ephemeral agent: its sandbox belongs to a task", func(ctx SpecContext) {
			// The suite's own agent has no backup after this run, so it is not archived either.
			dirtyHome(ctx, org, user, agent.ID)
			eph := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
			pass(ctx)
			now = time.Now().Add(30 * 24 * time.Hour)
			pass(ctx)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
			sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: eph.ID})
			if err == nil {
				Expect(sb.ArchivedAt.Valid).To(BeFalse())
			}
			Expect(archivedActions(ctx)).To(Equal(0))
		})

		It("archives a sandbox with a parked run, and creates one with the same generation when the park becomes claimable", func(ctx SpecContext) {
			lineage := row(ctx).HomeGeneration.String
			queueRun(ctx, org, user, agent.ID)
			run := claimRun(ctx, org)
			_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonApproval})
			Expect(err).NotTo(HaveOccurred())
			now = time.Now().Add(2 * time.Hour)
			pass(ctx)
			finish(ctx, true)
			pass(ctx)
			Expect(row(ctx).State).To(Equal(store.SandboxStateStopped))

			now = time.Now().Add(8 * 24 * time.Hour)
			creates := osg.Calls("CreateSandbox")
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
			Expect(row(ctx).ArchivedAt.Valid).To(BeTrue())
			pass(ctx)
			Expect(osg.Calls("CreateSandbox")).To(Equal(creates), "a park that is not claimable creates nothing")

			_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: run.ID})
			Expect(err).NotTo(HaveOccurred())
			pass(ctx)
			Expect(osg.Calls("CreateSandbox")).To(Equal(creates + 1))
			got := row(ctx)
			Expect(got.ArchivedAt.Valid).To(BeFalse())
			Expect(got.HomeGeneration.String).To(Equal(lineage))
		})

		It("does not archive when the archive tier is off", func(ctx SpecContext) {
			stopWith(ctx, true)
			cfg.ArchiveAfter = -1
			var err error
			r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
			Expect(err).NotTo(HaveOccurred())
			now = time.Now().Add(30 * 24 * time.Hour)
			pass(ctx)
			Expect(osg.Calls("DeleteSandbox")).To(Equal(0))
		})
	})
})
