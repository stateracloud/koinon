// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// racingEpochs runs before its first listing of an agent's epoch folders: a keys command that
// starts an epoch while the resync looks at the bucket.
type racingEpochs struct {
	agentd.HomeStore
	before func(ctx context.Context)
	once   *sync.Once
}

func (r racingEpochs) HeldEpochs(ctx context.Context, org, agent string) ([]int32, error) {
	r.once.Do(func() { r.before(ctx) })
	return r.HomeStore.HeldEpochs(ctx, org, agent)
}

// gatedListing holds the first listing until release is closed: a resync whose request is replaced
// while it lists the bucket.
type gatedListing struct {
	agentd.HomeStore
	started, release chan struct{}
	once             *sync.Once
}

func (g gatedListing) Snapshots(ctx context.Context, prefix, pw string) ([]checkpoint.Snapshot, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.release
	})
	return g.HomeStore.Snapshots(ctx, prefix, pw)
}

// recordingListing runs during after it listed the bucket: a backup that records its snapshot
// while the resync lists.
type recordingListing struct {
	agentd.HomeStore
	during func(ctx context.Context, snaps []checkpoint.Snapshot)
}

func (r recordingListing) Snapshots(ctx context.Context, prefix, pw string) ([]checkpoint.Snapshot, error) {
	snaps, err := r.HomeStore.Snapshots(ctx, prefix, pw)
	if err == nil {
		r.during(ctx, snaps)
	}
	return snaps, err
}

// checkpoints resync after a database restore (#89, ADR 0085 decision 17): home_checkpoints is
// rebuilt from the bucket.
var _ = Describe("home checkpoint resync", func() {
	var (
		e       *env
		writer  *checkpoint.Writer
		current *atomic.Int32
		lost    map[int32]bool
		m       *agentd.Metrics
		agent   pgtype.UUID
	)
	cps := func(extra ...func(*agentd.Checkpoints)) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.Metrics = m
			c.Checkpoints = &agentd.Checkpoints{
				Store: writer, Repositories: fixedRepo{current: current, lost: lost}, Interval: 24 * time.Hour,
				Poll: 50 * time.Millisecond, Sweep: time.Hour, Attempts: 2, Retry: 10 * time.Millisecond, Timeout: 20 * time.Second,
				Lease: 20 * time.Minute,
			}
			for _, x := range extra {
				x(c.Checkpoints)
			}
		}
	}
	password := func(epoch int32) string {
		r, err := fixedRepo{current: current}.At(context.Background(), e.org, agent, epoch)
		Expect(err).NotTo(HaveOccurred())
		return r.Password
	}
	prefix := func(epoch int32) string { return checkpoint.EpochPrefix(e.org.String(), agent.String(), epoch) }
	// snapshot writes a snapshot of files to the agent's repository epoch, as a backup does.
	snapshot := func(ctx context.Context, epoch int32, files map[string]string, meta checkpoint.Meta) checkpoint.Result {
		GinkgoHelper()
		res, err := writer.Backup(ctx, prefix(epoch), password(epoch), tarOf(files), meta)
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	// recorded is a complete checkpoint of a snapshot, finished ago before now: what the dump holds.
	recorded := func(ctx context.Context, epoch int32, snapshotID string, ago time.Duration) pgtype.UUID {
		GinkgoHelper()
		var id pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, size_bytes, generation,
				epoch, requested_at, started_at, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', $3, 1, 'gen-1', 0, now() - $4::interval, now() - $4::interval, now() - $4::interval, $5)
			RETURNING id`, e.org, agent, snapshotID, ago.String(), epoch).Scan(&id)).To(Succeed())
		return id
	}
	checkpointRow := func(ctx context.Context, id pgtype.UUID) store.HomeCheckpoint {
		GinkgoHelper()
		cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: id})
		Expect(err).NotTo(HaveOccurred())
		return cp
	}
	bySnapshot := func(ctx context.Context, snapshotID string) []store.HomeCheckpoint {
		GinkgoHelper()
		rows, err := db.Query(ctx, `SELECT id FROM home_checkpoints WHERE org_id = $1 AND agent_id = $2 AND snapshot_id = $3`, e.org, agent, snapshotID)
		Expect(err).NotTo(HaveOccurred())
		ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		Expect(err).NotTo(HaveOccurred())
		var out []store.HomeCheckpoint
		for _, id := range ids {
			out = append(out, checkpointRow(ctx, id))
		}
		return out
	}
	count := func(ctx context.Context) int {
		GinkgoHelper()
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM home_checkpoints WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&n)).To(Succeed())
		return n
	}
	request := func(ctx context.Context) store.HomeCheckpointResync {
		GinkgoHelper()
		r, err := q.RequestHomeCheckpointResync(ctx, store.RequestHomeCheckpointResyncParams{OrgID: e.org, AgentID: agent})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	resync := func(ctx context.Context) func() store.HomeCheckpointResync {
		return func() store.HomeCheckpointResync {
			var r store.HomeCheckpointResync
			Expect(db.QueryRow(ctx, `SELECT org_id, agent_id, request_id, requested_at, resync_after, attempts, error, finished_at, failed_at,
					snapshots, added, missing, skipped FROM home_checkpoint_resyncs WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(
				&r.OrgID, &r.AgentID, &r.RequestID, &r.RequestedAt, &r.ResyncAfter, &r.Attempts, &r.Error, &r.FinishedAt, &r.FailedAt,
				&r.Snapshots, &r.Added, &r.Missing, &r.Skipped)).To(Succeed())
			return r
		}
	}
	finished := func(ctx context.Context) func() bool {
		return func() bool { return resync(ctx)().FinishedAt.Valid }
	}
	taken := func(ctx context.Context) int32 {
		GinkgoHelper()
		var n pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT taken FROM home_checkpoint_resyncs WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&n)).To(Succeed())
		return n.Int32
	}
	counts := func(r store.HomeCheckpointResync) []int32 {
		return []int32{r.Snapshots.Int32, r.Added.Int32, r.Missing.Int32, r.Skipped.Int32}
	}
	audited := func(ctx context.Context, action string) []string {
		GinkgoHelper()
		rows, err := db.Query(ctx, `SELECT actor_kind || ' ' || target_id || ' ' || summary::text FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, action)
		Expect(err).NotTo(HaveOccurred())
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		return out
	}
	gauge := func(state string) func() float64 {
		return func() float64 { return testutil.ToFloat64(m.HomeCheckpointResyncs.WithLabelValues(state)) }
	}
	changes := func(change string) float64 {
		return testutil.ToFloat64(m.HomeCheckpointResyncChanges.WithLabelValues(change))
	}
	// newEpoch starts repository epoch n of the agent (keys forget or new-repositories did it),
	// retiring the one before with a deadline of in from now.
	newEpoch := func(ctx context.Context, n int32, in time.Duration) {
		GinkgoHelper()
		var err error
		if n == 1 {
			_, err = db.Exec(ctx, `UPDATE home_repositories SET retired_at = now(), delete_after = now() + $3::interval WHERE org_id = $1 AND agent_id = $2`, e.org, agent, in.String())
		} else {
			_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET retired_at = now(), delete_after = now() + $4::interval WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = $3`, e.org, agent, n-1, in.String())
		}
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, $3, $4, '{}', 'key_lost')`, e.org, agent, n, prefix(n))
		Expect(err).NotTo(HaveOccurred())
		current.Store(n)
	}

	BeforeEach(func(ctx SpecContext) {
		// The gauge counts every org in the database: resyncs earlier specs left pending (their pool
		// stopped first) are recorded as done, so only this spec's org counts.
		_, err := db.Exec(ctx, `UPDATE home_checkpoint_resyncs SET finished_at = now(), failed_at = NULL, snapshots = 0, added = 0, missing = 0, skipped = 0
			WHERE finished_at IS NULL`)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE home_checkpoint_resyncs SET failed_at = NULL WHERE failed_at IS NOT NULL`)
		Expect(err).NotTo(HaveOccurred())
		e = newEnv(ctx, store.AgentFlavorOpencode)
		writer = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
		current = &atomic.Int32{}
		lost = map[int32]bool{}
		m = agentd.NewMetrics(nil)
		// An agent without a sandbox: no live backup runs beside the resync.
		Expect(db.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, 'resynced', 'opencode') RETURNING id`, e.org).Scan(&agent)).To(Succeed())
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
			e.org, agent, checkpoint.Prefix(e.org.String(), agent.String()))
		Expect(err).NotTo(HaveOccurred())
	})

	It("adds the snapshots the database lacks and marks failed the checkpoints whose snapshot is gone; a second run changes nothing", func(ctx SpecContext) {
		By("a dump that names a snapshot retention deleted since, and one that is still there")
		kept := snapshot(ctx, 0, map[string]string{"notes/v.md": "kept"}, checkpoint.Meta{CheckpointID: "x"})
		keptID := recorded(ctx, 0, kept.SnapshotID, 3*time.Hour)
		goneID := recorded(ctx, 0, "k0123456789abcdef0123456789abcdef", 4*time.Hour)

		By("a backup written after the dump: its checkpoint row is not in the database")
		afterDump := pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true}
		newer := snapshot(ctx, 0, map[string]string{"notes/v.md": "after the dump"}, checkpoint.Meta{
			CheckpointID: afterDump.String(), Generation: "gen-2", Epoch: 7, Reason: "recreate", Sandbox: "sb-2",
		})
		request(ctx)
		stop := e.pool("agentd-a", cps())

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		r := resync(ctx)()
		Expect(counts(r)).To(Equal([]int32{2, 1, 1, 0}), "snapshots, added, missing, skipped")
		Expect(r.Attempts).To(BeEquivalentTo(1))

		By("the new snapshot is a complete checkpoint, with the ID, times and stamps it was written with")
		cp := checkpointRow(ctx, afterDump)
		Expect(cp.Status).To(Equal(store.CheckpointStatusComplete))
		Expect(cp.SnapshotID.String).To(Equal(newer.SnapshotID))
		Expect(cp.RepositoryEpoch).To(BeEquivalentTo(0))
		Expect(cp.Generation).To(Equal("gen-2"))
		Expect(cp.Epoch).To(BeEquivalentTo(7))
		Expect(cp.Reason).To(Equal(store.CheckpointReasonRecreate))
		Expect(cp.SandboxID).To(Equal("sb-2"))
		Expect(cp.SizeBytes.Int64).To(Equal(newer.Bytes))
		Expect(cp.FinishedAt.Time).To(BeTemporally("~", time.Now(), time.Minute))
		Expect(cp.StartedAt.Time).NotTo(BeTemporally(">", cp.FinishedAt.Time))

		By("the gone one is failed, not deleted, and names its snapshot; the kept one is as it was")
		gone := checkpointRow(ctx, goneID)
		Expect(gone.Status).To(Equal(store.CheckpointStatusFailed))
		Expect(gone.SnapshotID.Valid).To(BeFalse())
		Expect(gone.Error.String).To(ContainSubstring("k0123456789abcdef0123456789abcdef"))
		Expect(gone.FinishedAt.Valid).To(BeTrue())
		Expect(gone.SnapshotMissingAt.Valid).To(BeTrue(), "told apart from a failed backup (#263)")
		Expect(checkpointRow(ctx, keptID).Status).To(Equal(store.CheckpointStatusComplete))
		Expect(checkpointRow(ctx, keptID).SnapshotMissingAt.Valid).To(BeFalse())

		By("the agent's ownership epoch is raised to the newest added checkpoint's, as the dump had it lower")
		var agentEpoch int64
		Expect(db.QueryRow(ctx, `SELECT epoch FROM agents WHERE org_id = $1 AND id = $2`, e.org, agent).Scan(&agentEpoch)).To(Succeed())
		Expect(agentEpoch).To(BeEquivalentTo(7))

		By("audited by the system actor, in the agent's org; the gauge and the change counter follow")
		Expect(audited(ctx, agentd.ResyncedAction)).To(ConsistOf(And(HavePrefix("system "+agent.String()), ContainSubstring(`"added": 1`), ContainSubstring(`"missing": 1`))))
		Eventually(gauge("pending")).Should(BeZero())
		Expect(changes("added")).To(BeEquivalentTo(1))
		Expect(changes("missing")).To(BeEquivalentTo(1))

		By("a second request finds nothing to change")
		before := count(ctx)
		second := request(ctx)
		Expect(second.FinishedAt.Valid).To(BeFalse(), "a new request starts over")
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(counts(resync(ctx)())).To(Equal([]int32{2, 0, 0, 0}))
		Expect(count(ctx)).To(Equal(before))
		Expect(changes("added")).To(BeEquivalentTo(1))
		Expect(audited(ctx, agentd.ResyncedAction)).To(HaveLen(2))
		Expect(audited(ctx, agentd.ResyncedAction)).To(ContainElement(ContainSubstring(`"ownershipEpochRaisedTo": 7`)))

		By("the next run's restore takes the newest home, the one written after the dump")
		stop()
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: e.org, AgentID: agent, Config: []byte(`{"model":"m","instructions":"i"}`), CreatedBy: e.user,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: e.org, ID: agent, VersionID: version.ID, ActivatedBy: e.user})
		Expect(err).NotTo(HaveOccurred())
		t, err := q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: e.org, Title: uniq("task"), AgentID: agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: e.user,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: e.org, AgentID: agent, TaskID: t.ID, MaxWallClockSeconds: 1800})
		Expect(err).NotTo(HaveOccurred())
		run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(run.AgentID).To(Equal(agent))
		Expect(run.Epoch).To(BeEquivalentTo(8))
		all, err := q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{OrgID: e.org, AgentID: agent, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))
		Expect(all[0].ID).To(Equal(afterDump))
		Expect(all[1].ID).To(Equal(keptID))
	})

	It("records the prefixes it finds taken by repositories the database has no password for, and the free epoch after them (#262)", func(ctx SpecContext) {
		By("the dump knows epoch 1; after it, epoch 1 and 2 were started under passwords only the lost database had")
		newEpoch(ctx, 1, 7*24*time.Hour)
		for _, n := range []int32{1, 2} {
			_, err := writer.Backup(ctx, prefix(n), fmt.Sprintf("koinon_test_foreign-%d", n), tarOf(map[string]string{"notes/v.md": "after the dump"}),
				checkpoint.Meta{CheckpointID: "x"})
			Expect(err).NotTo(HaveOccurred())
		}
		home := snapshot(ctx, 0, map[string]string{"notes/v.md": "before"}, checkpoint.Meta{CheckpointID: "x"})
		recorded(ctx, 0, home.SnapshotID, time.Hour)
		request(ctx)
		e.pool("agentd-a", cps())

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		r := resync(ctx)()
		Expect(r.FailedAt.Valid).To(BeFalse(), r.Error.String)
		Expect(counts(r)).To(Equal([]int32{1, 0, 0, 0}), "epoch 1 is skipped, not listed under the wrong password")

		By("epoch 1's row is marked taken, and the next epoch starts at 3, past r2/ that no row names")
		var marked bool
		Expect(db.QueryRow(ctx, `SELECT prefix_taken_at IS NOT NULL FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`,
			e.org, agent).Scan(&marked)).To(Succeed())
		Expect(marked).To(BeTrue())
		var floor pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&floor)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 3, Valid: true}))
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(ConsistOf(
			And(HavePrefix("system "+agent.String()), ContainSubstring(`"repositoryEpoch": 1`), ContainSubstring(`"knownToDatabase": true`), ContainSubstring(`"nextEpochAtLeast": 3`)),
		))

		Expect(taken(ctx)).To(BeEquivalentTo(2), "r1/ and r2/")

		By("a second run finds the same and records nothing new")
		request(ctx)
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(HaveLen(1))
		Expect(taken(ctx)).To(BeEquivalentTo(2))
	})

	It("reports the repositories found after the latest epoch, past a gap, and counts them (#262)", func(ctx SpecContext) {
		// The lost database started epochs 1 to 3 and its sweeper deleted epoch 2's.
		for _, n := range []int32{1, 3} {
			_, err := writer.Backup(ctx, prefix(n), fmt.Sprintf("koinon_test_foreign-%d", n), tarOf(map[string]string{"notes/v.md": "after the dump"}),
				checkpoint.Meta{CheckpointID: "x"})
			Expect(err).NotTo(HaveOccurred())
		}
		request(ctx)
		e.pool("agentd-a", cps())

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		var floor pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&floor)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 4, Valid: true}), "past r3/, not at the gap r2/")
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(ConsistOf(
			And(ContainSubstring(`"repositoryEpoch": 1`), ContainSubstring(`"knownToDatabase": false`), ContainSubstring(prefix(1)), ContainSubstring(`"nextEpochAtLeast": 4`)),
		))
		Expect(taken(ctx)).To(BeEquivalentTo(2))
		Expect(audited(ctx, agentd.ResyncedAction)).To(ConsistOf(ContainSubstring(`"takenPrefixes": 2`)))
	})

	It("records nothing for a repository after the latest epoch that a keys command started while the resync listed (#262)", func(ctx SpecContext) {
		request(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = racingEpochs{HomeStore: writer, once: &sync.Once{}, before: func(ctx context.Context) {
				// keys forget commits epoch 1 after the resync read the epochs, and a backup writes there.
				newEpoch(ctx, 1, 7*24*time.Hour)
				snapshot(ctx, 1, map[string]string{"notes/v.md": "new epoch"}, checkpoint.Meta{CheckpointID: "x"})
			}}
		}))

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		var floor pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&floor)).To(Succeed())
		Expect(floor.Valid).To(BeFalse(), "the repository is the database's own")
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(BeEmpty())
		Expect(taken(ctx)).To(BeEquivalentTo(0))
	})

	It("re-checks every folder after the latest epoch under the lock, not the first only (#291)", func(ctx SpecContext) {
		// The lost database started epochs 1 and 2 (r1/ and r2/ hold repositories).
		for _, n := range []int32{1, 2} {
			_, err := writer.Backup(ctx, prefix(n), fmt.Sprintf("koinon_test_foreign-%d", n), tarOf(map[string]string{"notes/v.md": "after the dump"}),
				checkpoint.Meta{CheckpointID: "x"})
			Expect(err).NotTo(HaveOccurred())
		}
		request(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = racingEpochs{HomeStore: writer, once: &sync.Once{}, before: func(ctx context.Context) {
				// keys forget commits epoch 1 after the resync read the epochs; r2/ stays a foreign one.
				newEpoch(ctx, 1, 7*24*time.Hour)
			}}
		}))

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		var floor pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, agent).Scan(&floor)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 3, Valid: true}), "past r2/, which the first folder's re-check alone would have dropped")
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(ConsistOf(
			And(ContainSubstring(`"repositoryEpoch": 2`), ContainSubstring(`"nextEpochAtLeast": 3`)),
		))
		Expect(taken(ctx)).To(BeEquivalentTo(1), "r2/ only: r1/ is the database's own")
	})

	It("lists the bucket before it takes the agent's repository row lock (#290)", func(ctx SpecContext) {
		newEpoch(ctx, 1, 7*24*time.Hour)
		_, err := writer.Backup(ctx, prefix(1), "koinon_test_foreign-1", tarOf(map[string]string{"notes/v.md": "after the dump"}),
			checkpoint.Meta{CheckpointID: "x"})
		Expect(err).NotTo(HaveOccurred())
		var lockedDuringListing atomic.Bool
		request(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = racingEpochs{HomeStore: writer, once: &sync.Once{}, before: func(ctx context.Context) {
				// A listing under the lock would find the row locked by this transaction's own pool
				// connection; the probe is another one, so NOWAIT fails when the lock is held.
				tx, err := db.Begin(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, `SELECT 1 FROM home_repositories WHERE org_id = $1 AND agent_id = $2 FOR UPDATE NOWAIT`, e.org, agent); err != nil {
					lockedDuringListing.Store(true)
				}
			}}
		}))

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(lockedDuringListing.Load()).To(BeFalse(), "the bucket was listed while the row lock was held")
		Expect(audited(ctx, agentd.PrefixTakenAction)).NotTo(BeEmpty())
	})

	It("leaves out a snapshot whose backup is still recording it or was given up, and keeps a copy as old as its source", func(ctx SpecContext) {
		By("a running backup within its lease, and a request that is waiting to be tried again")
		var running, failed pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, started_at, attempts)
			VALUES ($1, $2, 'hourly', 'running', now(), 1) RETURNING id`, e.org, agent).Scan(&running)).To(Succeed())
		snapshot(ctx, 0, map[string]string{"notes/v.md": "running"}, checkpoint.Meta{CheckpointID: running.String()})
		Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, finished_at, error, attempts)
			VALUES ($1, $2, 'stop', 'failed', now(), 'tar exited with 2', 2) RETURNING id`, e.org, agent).Scan(&failed)).To(Succeed())
		snapshot(ctx, 0, map[string]string{"notes/v.md": "failed attempt"}, checkpoint.Meta{CheckpointID: failed.String()})

		By("a seed's copy in epoch 1 of a checkpoint the dump has in epoch 0")
		src := snapshot(ctx, 0, map[string]string{"notes/v.md": "source"}, checkpoint.Meta{CheckpointID: "x"})
		srcID := recorded(ctx, 0, src.SnapshotID, 2*time.Hour)
		source := checkpointRow(ctx, srcID)
		newEpoch(ctx, 1, 7*24*time.Hour)
		cp1 := snapshot(ctx, 1, map[string]string{"notes/v.md": "source"}, checkpoint.Meta{CheckpointID: srcID.String(), Generation: "gen-1"})

		By("a snapshot whose checkpoint tag names no checkpoint: adopted under a new ID")
		snapshot(ctx, 1, map[string]string{"notes/v.md": "odd"}, checkpoint.Meta{CheckpointID: "x"})

		request(ctx)
		e.pool("agentd-a", cps())
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(counts(resync(ctx)())).To(Equal([]int32{5, 2, 0, 2}), "snapshots (3 in epoch 0, 2 in epoch 1), added, missing, skipped")

		Expect(checkpointRow(ctx, running).Status).To(Equal(store.CheckpointStatusRunning), "its backup records it")
		Expect(checkpointRow(ctx, failed).Status).To(Equal(store.CheckpointStatusFailed))
		copies := bySnapshot(ctx, cp1.SnapshotID)
		Expect(copies).To(HaveLen(1))
		cp := copies[0]
		Expect(cp.ID).NotTo(Equal(srcID), "a new checkpoint, the source keeps its own")
		Expect(cp.RepositoryEpoch).To(BeEquivalentTo(1))
		Expect(cp.FinishedAt.Time).To(BeTemporally("==", source.FinishedAt.Time), "no newer a home than its source")
		Expect(cp.StartedAt.Time).To(BeTemporally("==", source.StartedAt.Time))
		Expect(cp.Reason).To(Equal(source.Reason))
	})

	It("keeps out another attempt's snapshot of a checkpoint: only the latest of a tag in a repository, none beside a complete one", func(ctx SpecContext) {
		By("a checkpoint the dump has complete, with a second snapshot under its tag: a retry of a binary before the Ready hook")
		good := snapshot(ctx, 0, map[string]string{"notes/v.md": "whole"}, checkpoint.Meta{CheckpointID: "x", Epoch: 2})
		completeID := recorded(ctx, 0, good.SnapshotID, time.Hour)
		partialBeside := snapshot(ctx, 0, map[string]string{"notes/v.md": "partial"}, checkpoint.Meta{CheckpointID: completeID.String(), Epoch: 2})

		By("a checkpoint written after the dump in two attempts, the first partial")
		afterDump := pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true}
		partial := snapshot(ctx, 0, map[string]string{"notes/v.md": "partial"}, checkpoint.Meta{CheckpointID: afterDump.String(), Epoch: 3})
		retry := snapshot(ctx, 0, map[string]string{"notes/v.md": "whole after the dump"}, checkpoint.Meta{CheckpointID: afterDump.String(), Epoch: 3})

		By("an agent whose runs are past the adopted epoch: it is not lowered")
		_, err := db.Exec(ctx, `UPDATE agents SET epoch = 9 WHERE org_id = $1 AND id = $2`, e.org, agent)
		Expect(err).NotTo(HaveOccurred())

		request(ctx)
		e.pool("agentd-a", cps())
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(counts(resync(ctx)())).To(Equal([]int32{4, 1, 0, 2}), "snapshots, added, missing, skipped")
		var agentEpoch int64
		Expect(db.QueryRow(ctx, `SELECT epoch FROM agents WHERE org_id = $1 AND id = $2`, e.org, agent).Scan(&agentEpoch)).To(Succeed())
		Expect(agentEpoch).To(BeEquivalentTo(9))
		Expect(bySnapshot(ctx, partialBeside.SnapshotID)).To(BeEmpty())
		Expect(bySnapshot(ctx, partial.SnapshotID)).To(BeEmpty())
		Expect(checkpointRow(ctx, afterDump).SnapshotID.String).To(Equal(retry.SnapshotID))
		Expect(checkpointRow(ctx, completeID).SnapshotID.String).To(Equal(good.SnapshotID))
	})

	It("skips lost and deleted epochs, and takes a swept repository as empty", func(ctx SpecContext) {
		By("epoch 0 retired past its deadline and swept from the bucket, though the dump predates the sweep")
		e0 := snapshot(ctx, 0, map[string]string{"notes/v.md": "epoch 0"}, checkpoint.Meta{CheckpointID: "x"})
		swept := recorded(ctx, 0, e0.SnapshotID, 3*time.Hour)
		newEpoch(ctx, 1, -time.Minute)
		_, err := writer.DeleteRepository(ctx, prefix(0))
		Expect(err).NotTo(HaveOccurred())

		By("epoch 1 lost with its key; epoch 2 current")
		e1 := snapshot(ctx, 1, map[string]string{"notes/v.md": "epoch 1"}, checkpoint.Meta{CheckpointID: "x"})
		inLost := recorded(ctx, 1, e1.SnapshotID, 2*time.Hour)
		newEpoch(ctx, 2, 7*24*time.Hour)
		_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET password_envelope = NULL, credential_lost_at = now() WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, e.org, agent)
		Expect(err).NotTo(HaveOccurred())
		lost[1] = true
		e2 := snapshot(ctx, 2, map[string]string{"notes/v.md": "epoch 2"}, checkpoint.Meta{CheckpointID: "x"})

		request(ctx)
		e.pool("agentd-a", cps())
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(counts(resync(ctx)())).To(Equal([]int32{1, 1, 1, 0}))
		Expect(checkpointRow(ctx, swept).Status).To(Equal(store.CheckpointStatusFailed))
		Expect(checkpointRow(ctx, inLost).Status).To(Equal(store.CheckpointStatusComplete), "a lost repository cannot be listed: left as it is")
		Expect(bySnapshot(ctx, e2.SnapshotID)).To(HaveLen(1))
	})

	It("changes nothing when a repository that is not due is missing from the bucket, and gives up after its attempts", func(ctx SpecContext) {
		id := recorded(ctx, 0, "k0123456789abcdef0123456789abcdef", time.Hour)
		request(ctx)
		e.pool("agentd-a", cps())
		Eventually(func() bool { return resync(ctx)().FailedAt.Valid }).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		r := resync(ctx)()
		Expect(r.Attempts).To(BeEquivalentTo(2))
		Expect(r.Error.String).To(ContainSubstring("not in the bucket"))
		Expect(checkpointRow(ctx, id).Status).To(Equal(store.CheckpointStatusComplete))
		Expect(audited(ctx, agentd.ResyncFailedAction)).To(ConsistOf(HavePrefix("system " + agent.String())))
		Eventually(gauge("failed")).Should(BeEquivalentTo(1))
		Expect(gauge("pending")()).To(BeZero())
	})

	It("waits while the agent's new repository waits for its seed", func(ctx SpecContext) {
		_, err := db.Exec(ctx, `UPDATE home_repositories SET retired_at = now(), delete_after = now() WHERE org_id = $1 AND agent_id = $2`, e.org, agent)
		Expect(err).NotTo(HaveOccurred())
		// A seed whose next try is far off: the pool's seed loop leaves it.
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, seed_from_epoch, seed_after)
			VALUES ($1, $2, 1, $3, '{}', 'key_compromised', 0, now() + interval '1 day')`, e.org, agent, prefix(1))
		Expect(err).NotTo(HaveOccurred())
		current.Store(1)
		request(ctx)
		e.pool("agentd-a", cps())
		Consistently(func() int32 { return resync(ctx)().Attempts }).WithTimeout(time.Second).Should(BeZero())
		Eventually(gauge("pending")).Should(BeEquivalentTo(1))

		_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET seeded_at = now() WHERE org_id = $1 AND agent_id = $2`, e.org, agent)
		Expect(err).NotTo(HaveOccurred())
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
	})

	It("neither duplicates nor marks missing a checkpoint a backup records while the resync lists the bucket", func(ctx SpecContext) {
		res := snapshot(ctx, 0, map[string]string{"notes/v.md": "recorded during the listing"}, checkpoint.Meta{CheckpointID: "x"})
		var live pgtype.UUID
		var once sync.Once
		during := func(ctx context.Context, snaps []checkpoint.Snapshot) {
			once.Do(func() { live = recorded(ctx, 0, res.SnapshotID, 0) })
		}
		request(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = recordingListing{HomeStore: writer, during: during} }))
		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		Expect(counts(resync(ctx)())).To(Equal([]int32{1, 0, 0, 0}), "the backup's row names it: not added, not missing")
		Expect(bySnapshot(ctx, res.SnapshotID)).To(HaveLen(1))
		Expect(checkpointRow(ctx, live).Status).To(Equal(store.CheckpointStatusComplete))
	})

	It("records nothing for a try whose request was replaced, and the new request resyncs", func(ctx SpecContext) {
		res := snapshot(ctx, 0, map[string]string{"notes/v.md": "after the dump"}, checkpoint.Meta{CheckpointID: "x"})
		gate := gatedListing{HomeStore: writer, started: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
		first := request(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = gate }))
		Eventually(gate.started).WithTimeout(racescale.D(20 * time.Second)).Should(BeClosed())
		second := request(ctx)
		Expect(second.RequestID).NotTo(Equal(first.RequestID))
		close(gate.release)

		Eventually(finished(ctx)).WithTimeout(racescale.D(20 * time.Second)).Should(BeTrue())
		r := resync(ctx)()
		Expect(r.RequestID).To(Equal(second.RequestID))
		Expect(r.Attempts).To(BeEquivalentTo(1), "the first try recorded no outcome")
		Expect(bySnapshot(ctx, res.SnapshotID)).To(HaveLen(1), "the snapshot is recorded once")
		Expect(audited(ctx, agentd.ResyncedAction)).To(HaveLen(1))
	})
})
