// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// gatedStore holds the first backup of checkpoint id until release is closed: a backup that is
// running while the agent's repository epoch changes.
type gatedStore struct {
	agentd.HomeStore
	id               string
	started, release chan struct{}
	once             *sync.Once
}

func (g gatedStore) Backup(ctx context.Context, prefix, pw string, r io.Reader, m checkpoint.Meta) (checkpoint.Result, error) {
	if m.CheckpointID == g.id {
		g.once.Do(func() {
			close(g.started)
			<-g.release
		})
	}
	return g.HomeStore.Backup(ctx, prefix, pw, r, m)
}

// Seeding a new home repository after keys new-repositories (#86, ADR 0085 decision 14).
var _ = Describe("home repository seeds", func() {
	const (
		pw0 = "koinon_test_not-a-real-token"
		pw1 = "koinon_test_not-a-real-token-1"
	)
	var (
		e       *env
		home    *fakeHome
		writer  *checkpoint.Writer
		current *atomic.Int32
		m       *agentd.Metrics
		e0, e1  string
	)
	cps := func(extra ...func(*agentd.Checkpoints)) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.Metrics = m
			c.Checkpoints = &agentd.Checkpoints{
				Store: writer, Repositories: fixedRepo{current: current}, Interval: 24 * time.Hour, Poll: 50 * time.Millisecond,
				Sweep: 50 * time.Millisecond, Attempts: 2, Retry: 10 * time.Millisecond, RestoreTries: 1, RestoreFallbacks: 2,
				Timeout: 20 * time.Second,
			}
			for _, x := range extra {
				x(c.Checkpoints)
			}
		}
	}
	// Each version of notes/v.md has its own length: the fake tar sets no mtime, and kopia takes a
	// file of the same size and mtime as its previous snapshot's for unchanged.
	//
	// backup writes a snapshot of files to repository epoch 0 and records it complete, finished ago
	// before now (ownership epoch 0: no run of the agent has a higher one, so a restore offers it).
	backup := func(ctx context.Context, files map[string]string, ago time.Duration) store.HomeCheckpoint {
		GinkgoHelper()
		res, err := writer.Backup(ctx, e0, pw0, tarOf(files), checkpoint.Meta{CheckpointID: "src", Generation: "gen-1"})
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, size_bytes, generation, epoch,
				started_at, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', $3, 1, 'gen-1', 0, now() - $4::interval, now() - $4::interval, 0) RETURNING id`,
			e.org, e.agent.ID, res.SnapshotID, ago.String()).Scan(&id)).To(Succeed())
		cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: id})
		Expect(err).NotTo(HaveOccurred())
		return cp
	}
	// newRepositories is what keys new-repositories leaves: epoch 0 retired with a deadline of now,
	// epoch 1 started and waiting for its seed.
	newRepositories := func(ctx context.Context) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `UPDATE home_repositories SET retired_at = now(), delete_after = now() WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, seed_from_epoch, seed_after)
			VALUES ($1, $2, 1, $3, '{}', 'key_compromised', 0, now())`, e.org, e.agent.ID, e1)
		Expect(err).NotTo(HaveOccurred())
		current.Store(1)
	}
	seedRow := func(ctx context.Context) func() store.HomeRepositoryEpoch {
		return func() store.HomeRepositoryEpoch {
			r, err := q.GetHomeRepositoryEpoch(ctx, store.GetHomeRepositoryEpochParams{OrgID: e.org, AgentID: e.agent.ID, RepositoryEpoch: 1})
			Expect(err).NotTo(HaveOccurred())
			return r
		}
	}
	seeded := func(ctx context.Context) func() bool {
		return func() bool { return seedRow(ctx)().SeededAt.Valid }
	}
	deleted0 := func(ctx context.Context) func() bool {
		return func() bool {
			var deleted pgtype.Timestamptz
			Expect(db.QueryRow(ctx, `SELECT deleted_at FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&deleted)).To(Succeed())
			return deleted.Valid
		}
	}
	// inEpoch1 is the agent's checkpoints in the new repository.
	inEpoch1 := func(ctx context.Context) []store.HomeCheckpoint {
		GinkgoHelper()
		all, err := q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{OrgID: e.org, AgentID: e.agent.ID, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		var out []store.HomeCheckpoint
		for _, c := range all {
			if c.RepositoryEpoch == 1 {
				out = append(out, c)
			}
		}
		return out
	}
	restored := func(ctx context.Context, prefix, pw, snapshot string) string {
		GinkgoHelper()
		var out bytes.Buffer
		Expect(writer.Restore(ctx, prefix, pw, snapshot, &out)).To(Succeed())
		return out.String()
	}
	audited := func(ctx context.Context, action string) []string {
		GinkgoHelper()
		rows, err := db.Query(ctx, `SELECT actor_kind || ' ' || target_id || ' ' || summary::text FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, action)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			Expect(rows.Scan(&s)).To(Succeed())
			out = append(out, s)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return out
	}
	gauge := func(state string) func() float64 {
		return func() float64 { return testutil.ToFloat64(m.HomeRepositorySeeds.WithLabelValues(state)) }
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, store.AgentFlavorOpencode)
		home = &fakeHome{files: map[string]string{"notes/v.md": "live"}}
		e.files.tar = home.tar
		e.gw.HandleUpload(home.upload)
		writer = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
		current = &atomic.Int32{}
		m = agentd.NewMetrics(nil)
		e0 = checkpoint.Prefix(e.org.String(), e.agent.ID.String())
		e1 = checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 1)
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`, e.org, e.agent.ID, e0)
		Expect(err).NotTo(HaveOccurred())
	})

	It("copies the newest checkpoint into the new repository, as old as its source, and then the old repository is deleted", func(ctx SpecContext) {
		backup(ctx, map[string]string{"notes/v.md": "old"}, 3*time.Minute)
		newest := backup(ctx, map[string]string{"notes/v.md": "newer home"}, 2*time.Minute)
		newRepositories(ctx)
		e.pool("agentd-a", cps())

		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		r := seedRow(ctx)()
		Expect(r.SeedAttempts).To(Equal(int32(1)))
		Expect(r.SeedError.Valid).To(BeFalse())
		copies := inEpoch1(ctx)
		Expect(copies).To(HaveLen(1))
		cp := copies[0]
		Expect(cp.FinishedAt.Time).To(BeTemporally("==", newest.FinishedAt.Time), "no newer a home than its source")
		Expect(cp.StartedAt.Time).To(BeTemporally("==", newest.StartedAt.Time))
		Expect(cp.Generation).To(Equal("gen-1"))
		Expect(cp.Reason).To(Equal(newest.Reason))
		Expect(restored(ctx, e1, pw1, cp.SnapshotID.String)).To(ContainSubstring("newer home"), "written to the new repository with its password")

		By("a restore tries the copy first")
		all, err := q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{OrgID: e.org, AgentID: e.agent.ID, MaxRows: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(all[0].ID).To(Equal(cp.ID))

		By("audited by the system actor, naming the source")
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(And(
			HavePrefix("system "+e.agent.ID.String()), ContainSubstring(newest.ID.String()), ContainSubstring(cp.ID.String()))))

		By("the new repository holds the newest home: the sweeper deletes the old one")
		Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(writer.Restore(ctx, e0, pw0, newest.SnapshotID.String, io.Discard)).NotTo(Succeed(), "the old repository's blobs are gone")
		Expect(gauge("pending")()).To(BeZero())
		Expect(gauge("failed")()).To(BeZero())
	})

	It("falls back to an older checkpoint when the newest does not copy", func(ctx SpecContext) {
		older := backup(ctx, map[string]string{"notes/v.md": "old"}, 3*time.Minute)
		newest := backup(ctx, map[string]string{"notes/v.md": "newer home"}, 2*time.Minute)
		newRepositories(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: newest.SnapshotID.String}
		}))

		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		copies := inEpoch1(ctx)
		Expect(copies).To(HaveLen(1))
		Expect(copies[0].FinishedAt.Time).To(BeTemporally("==", older.FinishedAt.Time))
		Expect(restored(ctx, e1, pw1, copies[0].SnapshotID.String)).To(ContainSubstring("old"))
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(ContainSubstring(older.ID.String())))

		By("the old repository's newest checkpoint is newer than the copy: kept until the new one holds a newer home")
		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse())
		_, err := db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
			VALUES ($1, $2, 'hourly', 'complete', 'later', now(), 1)`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
	})

	It("gives up after its attempts, audits it and keeps the old repository", func(ctx SpecContext) {
		newest := backup(ctx, map[string]string{"notes/v.md": "newer home"}, 2*time.Minute)
		newRepositories(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, backups: errors.New("the bucket refused the write")}
		}))

		Eventually(func() bool { return seedRow(ctx)().SeedFailedAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
		r := seedRow(ctx)()
		Expect(r.SeedAttempts).To(Equal(int32(2)))
		Expect(r.SeededAt.Valid).To(BeFalse())
		Expect(r.SeedError.String).To(ContainSubstring("the bucket refused the write"))
		Expect(inEpoch1(ctx)).To(BeEmpty())
		Expect(audited(ctx, agentd.SeedFailedAction)).To(ConsistOf(And(HavePrefix("system "), ContainSubstring("the bucket refused the write"))))
		Eventually(gauge("failed")).WithTimeout(5 * time.Second).Should(Equal(1.0))
		Expect(gauge("pending")()).To(BeZero())

		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse(), "nothing newer holds the home")
		Expect(restored(ctx, e0, pw0, newest.SnapshotID.String)).To(ContainSubstring("newer home"))
		Expect(seedRow(ctx)().SeedAttempts).To(Equal(int32(2)), "not tried again")
	})

	It("keeps the old repository while a backup that opened it runs, and while that backup is the newest home", func(ctx SpecContext) {
		backup(ctx, map[string]string{"notes/v.md": "v1"}, 2*time.Minute)
		home.set(map[string]string{"notes/v.md": "v2 (longer)"})
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		running, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
			OrgID: e.org, AgentID: e.agent.ID, Reason: store.CheckpointReasonStop, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String,
		})
		Expect(err).NotTo(HaveOccurred())
		gate := gatedStore{HomeStore: writer, id: running.ID.String(), started: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
		DeferCleanup(func() {
			select {
			case <-gate.release:
			default:
				close(gate.release)
			}
		})
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = gate }))
		Eventually(gate.started).WithTimeout(20 * time.Second).Should(BeClosed())

		By("the repository epoch changes while the backup, into epoch 0, runs: the seed copies v1")
		newRepositories(ctx)
		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		Expect(restored(ctx, e1, pw1, inEpoch1(ctx)[0].SnapshotID.String)).To(ContainSubstring("v1"))
		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse(), "a checkpoint of the agent is running")

		By("the backup finishes into epoch 0: its home is newer than the copy, so epoch 0 is kept")
		close(gate.release)
		Eventually(func() store.CheckpointStatus {
			cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: running.ID})
			Expect(err).NotTo(HaveOccurred())
			return cp.Status
		}).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusComplete))
		done, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: running.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(done.RepositoryEpoch).To(BeZero())
		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse())
		Expect(restored(ctx, e0, pw0, done.SnapshotID.String)).To(ContainSubstring("v2 (longer)"))

		By("the next backup goes to epoch 1: epoch 0 goes")
		home.set(map[string]string{"notes/v.md": "v3 (longest yet)"})
		next, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
			OrgID: e.org, AgentID: e.agent.ID, Reason: store.CheckpointReasonStop, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String,
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int32 {
			cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: next.ID})
			Expect(err).NotTo(HaveOccurred())
			if cp.Status != store.CheckpointStatusComplete {
				return -1
			}
			return cp.RepositoryEpoch
		}).WithTimeout(20 * time.Second).Should(Equal(int32(1)))
		Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
	})
})
