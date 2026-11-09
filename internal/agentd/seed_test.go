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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// gatedStore holds the first backup of checkpoint id until release is closed: a backup that is
// running while the agent's repository epoch changes. done is closed when that held try has
// verified its copy, which is the last store call before the seed records its outcome and frees
// the slot. With one seed slot the next try is claimed only then, so a spec waits on done first
// and each later Eventually budgets one try instead of two (#409).
type gatedStore struct {
	agentd.HomeStore
	id                     string
	started, release, done chan struct{}
	once                   *sync.Once
	snap                   *atomic.Value // the snapshot ID the held Backup wrote
}

func (g gatedStore) Backup(ctx context.Context, prefix, pw string, r io.Reader, m checkpoint.Meta) (checkpoint.Result, error) {
	if m.CheckpointID != g.id {
		return g.HomeStore.Backup(ctx, prefix, pw, r, m)
	}
	held := false
	g.once.Do(func() {
		held = true
		close(g.started)
		<-g.release
	})
	res, err := g.HomeStore.Backup(ctx, prefix, pw, r, m)
	if held {
		if err != nil {
			close(g.done)
		} else {
			g.snap.Store(res.SnapshotID)
		}
	}
	return res, err
}

func (g gatedStore) Verify(ctx context.Context, prefix, pw, snapshotID string) error {
	err := g.HomeStore.Verify(ctx, prefix, pw, snapshotID)
	if id, _ := g.snap.Load().(string); id != "" && id == snapshotID {
		g.snap.Store("")
		close(g.done)
	}
	return err
}

// flakyStore fails the first fail backups, then writes.
type flakyStore struct {
	agentd.HomeStore
	fail  int32
	calls atomic.Int32
}

func (f *flakyStore) Backup(ctx context.Context, prefix, pw string, r io.Reader, m checkpoint.Meta) (checkpoint.Result, error) {
	if f.calls.Add(1) <= f.fail {
		return checkpoint.Result{}, errors.New("the bucket was briefly unreachable")
	}
	return f.HomeStore.Backup(ctx, prefix, pw, r, m)
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
	backupOf := func(ctx context.Context, agent pgtype.UUID, files map[string]string, ago time.Duration) store.HomeCheckpoint {
		GinkgoHelper()
		prefix := checkpoint.Prefix(e.org.String(), agent.String())
		res, err := writer.Backup(ctx, prefix, pw0, tarOf(files), checkpoint.Meta{CheckpointID: "src", Generation: "gen-1"})
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, size_bytes, generation, epoch,
				started_at, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', $3, 1, 'gen-1', 0, now() - $4::interval, now() - $4::interval, 0) RETURNING id`,
			e.org, agent, res.SnapshotID, ago.String()).Scan(&id)).To(Succeed())
		cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: id})
		Expect(err).NotTo(HaveOccurred())
		return cp
	}
	backup := func(ctx context.Context, files map[string]string, ago time.Duration) store.HomeCheckpoint {
		GinkgoHelper()
		return backupOf(ctx, e.agent.ID, files, ago)
	}
	// newRepositoriesOf is what keys new-repositories leaves: the agent's current epoch (from)
	// retired with a deadline of now, the next one started and waiting for its seed.
	newRepositoriesOf := func(ctx context.Context, agent pgtype.UUID, from int32) {
		GinkgoHelper()
		var err error
		if from == 0 {
			_, err = db.Exec(ctx, `UPDATE home_repositories SET retired_at = now(), delete_after = now() WHERE org_id = $1 AND agent_id = $2`, e.org, agent)
		} else {
			_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET retired_at = now(), delete_after = now() WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = $3`, e.org, agent, from)
		}
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, seed_from_epoch, seed_after)
			VALUES ($1, $2, $3, $4, '{}', 'key_compromised', $5, now())`,
			e.org, agent, from+1, checkpoint.EpochPrefix(e.org.String(), agent.String(), from+1), from)
		Expect(err).NotTo(HaveOccurred())
		if current.Load() < from+1 {
			current.Store(from + 1)
		}
	}
	newRepositories := func(ctx context.Context) {
		GinkgoHelper()
		newRepositoriesOf(ctx, e.agent.ID, 0)
	}
	// secondAgent is another agent of the org, with a home repository (epoch 0) and no sandbox: a
	// seed needs none.
	secondAgent := func(ctx context.Context) pgtype.UUID {
		GinkgoHelper()
		var b pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, 'second', 'opencode') RETURNING id`, e.org).Scan(&b)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
			e.org, b, checkpoint.Prefix(e.org.String(), b.String()))
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	seedRowOf := func(ctx context.Context, agent pgtype.UUID, epoch int32) func() store.HomeRepositoryEpoch {
		return func() store.HomeRepositoryEpoch {
			r, err := q.GetHomeRepositoryEpoch(ctx, store.GetHomeRepositoryEpochParams{OrgID: e.org, AgentID: agent, RepositoryEpoch: epoch})
			Expect(err).NotTo(HaveOccurred())
			return r
		}
	}
	seedRow := func(ctx context.Context) func() store.HomeRepositoryEpoch {
		return seedRowOf(ctx, e.agent.ID, 1)
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
	// inEpoch is the agent's complete checkpoints in one repository epoch.
	inEpoch := func(ctx context.Context, agent pgtype.UUID, epoch int32) []store.HomeCheckpoint {
		GinkgoHelper()
		rows, err := db.Query(ctx, `SELECT id FROM home_checkpoints WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = $3 AND status = 'complete'`,
			e.org, agent, epoch)
		Expect(err).NotTo(HaveOccurred())
		ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		Expect(err).NotTo(HaveOccurred())
		var out []store.HomeCheckpoint
		for _, id := range ids {
			cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: id})
			Expect(err).NotTo(HaveOccurred())
			out = append(out, cp)
		}
		return out
	}
	// inEpoch1 is the agent's checkpoints in the new repository.
	inEpoch1 := func(ctx context.Context) []store.HomeCheckpoint {
		GinkgoHelper()
		return inEpoch(ctx, e.agent.ID, 1)
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
		// The seed gauge counts every org in the database, and the orgs of earlier specs stay: one
		// that ended with a seed still pending (its pool stopped first) or given up would count in
		// this spec's gauge. Their seeds are recorded as done, so only this spec's org counts.
		_, err := db.Exec(ctx, `UPDATE home_repository_epochs SET seeded_at = now(), seed_failed_at = NULL
			WHERE seed_from_epoch IS NOT NULL AND seeded_at IS NULL`)
		Expect(err).NotTo(HaveOccurred())
		e = newEnv(ctx, store.AgentFlavorOpencode)
		home = &fakeHome{files: map[string]string{"notes/v.md": "live"}}
		e.files.tar = home.tar
		e.gw.HandleUpload(home.upload)
		writer = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
		current = &atomic.Int32{}
		m = agentd.NewMetrics(nil)
		e0 = checkpoint.Prefix(e.org.String(), e.agent.ID.String())
		e1 = checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 1)
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`, e.org, e.agent.ID, e0)
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

		By("a backup into the new repository holds the home: the old one goes and the seed no longer counts as failed")
		_, err := db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
			VALUES ($1, $2, 'hourly', 'complete', 'later', now(), 1)`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(gauge("failed")).WithTimeout(5 * time.Second).Should(BeZero())
		Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
	})

	It("gives up at once on a seed into a prefix that holds another password's repository, records the free epoch and keeps the source (#262)", func(ctx SpecContext) {
		newest := backup(ctx, map[string]string{"notes/v.md": "newer home"}, 2*time.Minute)
		// The new epoch's prefix holds a repository started after the dump the database came from.
		foreign, err := writer.Backup(ctx, e1, "koinon_test_foreign-1", tarOf(map[string]string{"notes/v.md": "after the dump"}), checkpoint.Meta{CheckpointID: "x"})
		Expect(err).NotTo(HaveOccurred())
		newRepositories(ctx)
		e.pool("agentd-a", cps())

		Eventually(func() bool { return seedRow(ctx)().SeedFailedAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
		r := seedRow(ctx)()
		Expect(r.SeedAttempts).To(Equal(int32(1)), "another try cannot help")
		Expect(r.SeedError.String).To(And(ContainSubstring(e1), ContainSubstring("keys new-repositories --agent "+e.agent.ID.String()), ContainSubstring("free epoch 2")))
		Expect(r.PrefixTakenAt.Valid).To(BeTrue())
		var floor pgtype.Int4
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&floor)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 2, Valid: true}))
		Expect(audited(ctx, agentd.PrefixTakenAction)).To(ConsistOf(ContainSubstring(`"repositoryEpoch": 1`)))
		Expect(inEpoch1(ctx)).To(BeEmpty())

		By("the source is kept, and so is the repository found")
		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse())
		Expect(restored(ctx, e0, pw0, newest.SnapshotID.String)).To(ContainSubstring("newer home"))
		Expect(restored(ctx, e1, "koinon_test_foreign-1", foreign.SnapshotID)).To(ContainSubstring("after the dump"))
	})

	It("tries again after a failed try and then records the seed", func(ctx SpecContext) {
		newest := backup(ctx, map[string]string{"notes/v.md": "newer home"}, 2*time.Minute)
		newRepositories(ctx)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = &flakyStore{HomeStore: writer, fail: 1}
		}))

		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		r := seedRow(ctx)()
		Expect(r.SeedAttempts).To(Equal(int32(2)))
		Expect(r.SeedError.Valid).To(BeFalse(), "the error of the failed try is cleared")
		copies := inEpoch1(ctx)
		Expect(copies).To(HaveLen(1))
		Expect(restored(ctx, e1, pw1, copies[0].SnapshotID.String)).To(ContainSubstring("newer home"))
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(ContainSubstring(newest.ID.String())))
		Expect(audited(ctx, agentd.SeedFailedAction)).To(BeEmpty())
	})

	It("records the seed of an agent that has no checkpoint, with nothing to copy", func(ctx SpecContext) {
		newRepositories(ctx)
		e.pool("agentd-a", cps())

		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		Expect(seedRow(ctx)().SeedAttempts).To(Equal(int32(1)))
		Expect(inEpoch1(ctx)).To(BeEmpty())
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(And(HavePrefix("system "+e.agent.ID.String()), Not(ContainSubstring("fromCheckpoint")))))
		Eventually(gauge("pending")).WithTimeout(5 * time.Second).Should(BeZero())
	})

	It("gives up on one agent's seed and still seeds another's", func(ctx SpecContext) {
		bad := backup(ctx, map[string]string{"notes/v.md": "corrupt"}, 2*time.Minute)
		b := secondAgent(ctx)
		good := backupOf(ctx, b, map[string]string{"notes/v.md": "b's home"}, 2*time.Minute)
		newRepositories(ctx)
		newRepositoriesOf(ctx, b, 0)
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: bad.SnapshotID.String}
		}))

		Eventually(func() bool { return seedRow(ctx)().SeedFailedAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
		Eventually(func() bool { return seedRowOf(ctx, b, 1)().SeededAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
		Expect(inEpoch1(ctx)).To(BeEmpty())
		copies := inEpoch(ctx, b, 1)
		Expect(copies).To(HaveLen(1))
		Expect(restored(ctx, checkpoint.EpochPrefix(e.org.String(), b.String(), 1), pw1, copies[0].SnapshotID.String)).To(ContainSubstring("b's home"))
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(And(HavePrefix("system "+b.String()), ContainSubstring(good.ID.String()))))
		Expect(audited(ctx, agentd.SeedFailedAction)).To(ConsistOf(HavePrefix("system " + e.agent.ID.String())))
		Eventually(gauge("failed")).WithTimeout(5 * time.Second).Should(Equal(1.0))
		Expect(gauge("pending")()).To(BeZero())
		Consistently(deleted0(ctx)).WithTimeout(time.Second).Should(BeFalse(), "the agent whose seed failed keeps its old repository")
	})

	It("drops a seed superseded by a later new-repositories run and seeds the later epoch", func(ctx SpecContext) {
		src := backup(ctx, map[string]string{"notes/v.md": "the home"}, 2*time.Minute)
		newRepositories(ctx)
		gate := gatedStore{HomeStore: writer, id: src.ID.String(), started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), once: &sync.Once{}, snap: &atomic.Value{}}
		DeferCleanup(func() {
			select {
			case <-gate.release:
			default:
				close(gate.release)
			}
		})
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = gate }))
		Eventually(gate.started).WithTimeout(20 * time.Second).Should(BeClosed())

		By("keys new-repositories runs again while epoch 1's seed copies")
		newRepositoriesOf(ctx, e.agent.ID, 1)
		close(gate.release)
		By("the superseded try ends, recording nothing, and frees the slot before the next claim can start")
		Eventually(gate.done).WithTimeout(20 * time.Second).Should(BeClosed())
		Eventually(func() bool { return seedRowOf(ctx, e.agent.ID, 2)().SeededAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
		Expect(seedRow(ctx)().SeededAt.Valid).To(BeFalse(), "epoch 1 was superseded")
		Expect(inEpoch1(ctx)).To(BeEmpty(), "its copy is not recorded")
		copies := inEpoch(ctx, e.agent.ID, 2)
		Expect(copies).To(HaveLen(1))
		Expect(copies[0].FinishedAt.Time).To(BeTemporally("==", src.FinishedAt.Time))
		Expect(restored(ctx, checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 2), pw0+"-2", copies[0].SnapshotID.String)).To(ContainSubstring("the home"))
		Expect(audited(ctx, agentd.SeededAction)).To(ConsistOf(ContainSubstring(`"repositoryEpoch": 2`)))

		By("both older repositories go")
		Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
		Eventually(func() bool { return seedRow(ctx)().DeletedAt.Valid }).WithTimeout(10 * time.Second).Should(BeTrue())
	})

	It("records nothing for a try that outlived its claim, and the later claim seeds", func(ctx SpecContext) {
		src := backup(ctx, map[string]string{"notes/v.md": "the home"}, 2*time.Minute)
		newRepositories(ctx)
		gate := gatedStore{HomeStore: writer, id: src.ID.String(), started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), once: &sync.Once{}, snap: &atomic.Value{}}
		DeferCleanup(func() {
			select {
			case <-gate.release:
			default:
				close(gate.release)
			}
		})
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = gate }))
		Eventually(gate.started).WithTimeout(20 * time.Second).Should(BeClosed())

		By("another writer claims the seed while this try runs (its lease ran out)")
		_, err := db.Exec(ctx, `UPDATE home_repository_epochs SET seed_attempts = seed_attempts + 1, seed_after = now() + interval '1 hour'
			WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		close(gate.release)
		By("the stale try ends, then nothing is recorded for it")
		Eventually(gate.done).WithTimeout(20 * time.Second).Should(BeClosed())
		Consistently(seeded(ctx)).WithTimeout(2 * time.Second).Should(BeFalse())
		Expect(inEpoch1(ctx)).To(BeEmpty(), "the stale try's copy is not recorded")
		r := seedRow(ctx)()
		Expect(r.SeedAttempts).To(Equal(int32(2)))
		Expect(r.SeedError.Valid || r.SeedFailedAt.Valid).To(BeFalse(), "nor released or given up under the later claim")
		Expect(audited(ctx, agentd.SeededAction)).To(BeEmpty())

		By("the later claim's lease runs out too: the next try records the seed")
		_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET seed_after = now() WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
		Expect(seedRow(ctx)().SeedAttempts).To(Equal(int32(3)))
		Expect(inEpoch1(ctx)).To(HaveLen(1))
	})

	It("leaves a backup slot to the checkpoints while seeds run", func(ctx SpecContext) {
		src := backup(ctx, map[string]string{"notes/v.md": "the home"}, 2*time.Minute)
		b := secondAgent(ctx)
		backupOf(ctx, b, map[string]string{"notes/v.md": "b's home"}, 2*time.Minute)
		newRepositories(ctx)
		newRepositoriesOf(ctx, b, 0)
		// b's seed is due later, so the first seed claimed is the gated one.
		_, err := db.Exec(ctx, `UPDATE home_repository_epochs SET seed_after = now() + interval '1 second' WHERE org_id = $1 AND agent_id = $2`, e.org, b)
		Expect(err).NotTo(HaveOccurred())
		gate := gatedStore{HomeStore: writer, id: src.ID.String(), started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), once: &sync.Once{}, snap: &atomic.Value{}}
		DeferCleanup(func() {
			select {
			case <-gate.release:
			default:
				close(gate.release)
			}
		})
		e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Store = gate; c.Concurrency = 2 }))
		Eventually(gate.started).WithTimeout(20 * time.Second).Should(BeClosed())

		By("with 2 writers, one seed runs at a time: b's waits, and a stop checkpoint gets the other writer")
		Consistently(func() int32 { return seedRowOf(ctx, b, 1)().SeedAttempts }).WithTimeout(2 * time.Second).Should(BeZero())
		home.set(map[string]string{"notes/v.md": "live home"})
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		stop, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
			OrgID: e.org, AgentID: e.agent.ID, Reason: store.CheckpointReasonStop, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String,
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() store.CheckpointStatus {
			cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: stop.ID})
			Expect(err).NotTo(HaveOccurred())
			return cp.Status
		}).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusComplete))

		close(gate.release)
		By("the gated try ends and frees the one seed slot before b's seed can start")
		Eventually(gate.done).WithTimeout(20 * time.Second).Should(BeClosed())
		Eventually(func() bool { return seedRowOf(ctx, b, 1)().SeededAt.Valid }).WithTimeout(20 * time.Second).Should(BeTrue())
	})

	Describe("a checkpoint whose writer died while the sandbox is not ready", func() {
		var dead pgtype.UUID
		BeforeEach(func(ctx SpecContext) {
			backup(ctx, map[string]string{"notes/v.md": "the home"}, 3*time.Hour)
			// agentd died during a stop checkpoint two hours ago; the agent is stopped since, so no
			// writer takes the checkpoint over (that needs a ready sandbox).
			_, err := db.Exec(ctx, `UPDATE agent_sandboxes SET state = 'stopped' WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, attempts, requested_at, started_at)
				VALUES ($1, $2, 'stop', 'running', 1, now() - interval '2 hours', now() - interval '2 hours') RETURNING id`, e.org, e.agent.ID).Scan(&dead)).To(Succeed())
			newRepositories(ctx)
		})
		status := func(ctx context.Context) func() store.HomeCheckpoint {
			return func() store.HomeCheckpoint {
				cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: dead})
				Expect(err).NotTo(HaveOccurred())
				return cp
			}
		}

		It("holds nothing back past its lease: the old repository is deleted", func(ctx SpecContext) {
			// A long sandbox wait, so the checkpoint is still running when the sweeper looks.
			e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Lease = 30 * time.Minute; c.SandboxWait = 24 * time.Hour }))
			Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
			Eventually(deleted0(ctx)).WithTimeout(10 * time.Second).Should(BeTrue())
			Expect(status(ctx)().Status).To(Equal(store.CheckpointStatusRunning))
		})

		It("is failed after its lease and the sandbox wait", func(ctx SpecContext) {
			e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Lease = 30 * time.Minute; c.SandboxWait = 30 * time.Minute }))
			Eventually(status(ctx)).WithTimeout(10 * time.Second).Should(And(
				HaveField("Status", store.CheckpointStatusFailed),
				HaveField("Error.String", ContainSubstring("the writer stopped")),
			))
		})

		It("is left running within its lease and the sandbox wait", func(ctx SpecContext) {
			e.pool("agentd-a", cps(func(c *agentd.Checkpoints) { c.Lease = 30 * time.Minute; c.SandboxWait = 2 * time.Hour }))
			Eventually(seeded(ctx)).WithTimeout(20 * time.Second).Should(BeTrue())
			Consistently(status(ctx)).WithTimeout(time.Second).Should(HaveField("Status", store.CheckpointStatusRunning))
		})
	})
})
