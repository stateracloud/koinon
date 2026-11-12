// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
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
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// fakeHome is the sandbox's home as the fake tar sees it: paths under /sandbox and their bytes.
type fakeHome struct {
	mu    sync.Mutex
	files map[string]string
	// failRead makes the backup's tar fail with exit code 2, after it wrote an archive of the files
	// it could read (as GNU tar does on a read error).
	failRead bool
	// dests and names are what the uploads extracted: their destination directories and the entry
	// names of the archives.
	dests, names []string
}

// upload answers the upload channel's `cat | tar xf - -C dest`.
func (h *fakeHome) upload(_ context.Context, dest string, archive io.Reader, stderr io.Writer) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.files == nil {
		h.files = map[string]string{}
	}
	h.dests = append(h.dests, dest)
	tr := tar.NewReader(archive)
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			_, _ = io.WriteString(stderr, "tar: "+err.Error())
			return 2
		}
		h.names = append(h.names, hd.Name)
		if hd.Typeflag != tar.TypeReg {
			continue
		}
		b, _ := io.ReadAll(tr)
		h.files[hd.Name] = string(b)
	}
}

func (h *fakeHome) set(files map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files = files
}

func (h *fakeHome) get() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for k, v := range h.files {
		out[k] = v
	}
	return out
}

// tar answers `tar -C /sandbox ... -cf - .` (the backup). The restore does not run tar over exec:
// it goes through the upload channel, which upload answers.
func (h *fakeHome) tar(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case slices.Contains(args, "-cf"):
		tw := tar.NewWriter(stdout)
		_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
		for name, body := range h.files {
			_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			_, _ = io.WriteString(tw, body)
		}
		_ = tw.Close()
		if h.failRead {
			_, _ = io.WriteString(stderr, "tar: /sandbox: Input/output error")
			return 2
		}
		return 0
	}
	_, _ = io.WriteString(stderr, "tar: unexpected arguments")
	return 2
}

// fixedRepo gives each agent repository epochs 0 to *current, each at its own prefix with its own
// password (epoch 0's is koinon_test_not-a-real-token, epoch N's has -N appended); a nil current
// is epoch 0 only.
type fixedRepo struct {
	current *atomic.Int32
	lost    map[int32]bool // epochs whose password was lost with its key
}

func (f fixedRepo) Get(ctx context.Context, org, agent pgtype.UUID) (checkpoint.Repository, error) {
	var cur int32
	if f.current != nil {
		cur = f.current.Load()
	}
	return f.At(ctx, org, agent, cur)
}

func (f fixedRepo) At(_ context.Context, org, agent pgtype.UUID, epoch int32) (checkpoint.Repository, error) {
	if epoch < 0 || (epoch > 0 && (f.current == nil || epoch > f.current.Load())) {
		return checkpoint.Repository{}, checkpoint.ErrNoRepository
	}
	if f.lost[epoch] {
		return checkpoint.Repository{}, checkpoint.ErrRepositoryLost
	}
	pw := "koinon_test_not-a-real-token"
	if epoch > 0 {
		pw = fmt.Sprintf("%s-%d", pw, epoch)
	}
	return checkpoint.Repository{Epoch: epoch, Prefix: checkpoint.EpochPrefix(org.String(), agent.String(), epoch), Password: pw}, nil
}

// tarOf is a home archive holding files, as tar in the sandbox writes it.
func tarOf(files map[string]string) io.Reader {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range files {
		Expect(tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})).To(Succeed())
		_, err := io.WriteString(tw, body)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(tw.Close()).To(Succeed())
	return &b
}

// hangingRepo is a repository source whose Get (the call that may reach a KMS) returns only when its
// context ends.
type hangingRepo struct{ fixedRepo }

func (hangingRepo) Get(ctx context.Context, _, _ pgtype.UUID) (checkpoint.Repository, error) {
	<-ctx.Done()
	return checkpoint.Repository{}, ctx.Err()
}

// failingStore is a checkpoint store whose backups, or restores of one snapshot, fail.
type failingStore struct {
	agentd.HomeStore
	backups     error
	failRestore string
}

func (f failingStore) Backup(ctx context.Context, prefix, pw string, r io.Reader, m checkpoint.Meta) (checkpoint.Result, error) {
	if f.backups != nil {
		return checkpoint.Result{}, f.backups
	}
	return f.HomeStore.Backup(ctx, prefix, pw, r, m)
}

func (f failingStore) Restore(ctx context.Context, prefix, pw, snap string, w io.Writer) error {
	if snap == f.failRestore {
		return errors.New("the snapshot is corrupt")
	}
	return f.HomeStore.Restore(ctx, prefix, pw, snap, w)
}

// blockingStore is a checkpoint store whose restore stages for a long time, as kopia does before it
// writes the first byte: it returns only when its context ends.
type blockingStore struct {
	agentd.HomeStore
	stopped chan struct{}
}

func (b blockingStore) Restore(ctx context.Context, _, _, _ string, _ io.Writer) error {
	<-ctx.Done()
	close(b.stopped)
	return ctx.Err()
}

// lateStore is a checkpoint store whose backup finishes the snapshot, then returns only after its
// context has expired, as one that completes just past the attempt deadline does. It reads the
// whole stream first, on the attempt context: the exec stdout pipe is closed when that context
// ends. The repository work (kopia opens, scrypt included) then runs detached from the deadline,
// so its duration, which depends on build tags and machine load, cannot decide the spec (#638).
type lateStore struct{ agentd.HomeStore }

func (l lateStore) Backup(ctx context.Context, prefix, pw string, r io.Reader, m checkpoint.Meta) (checkpoint.Result, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return checkpoint.Result{}, err
	}
	res, err := l.HomeStore.Backup(context.WithoutCancel(ctx), prefix, pw, bytes.NewReader(data), m)
	<-ctx.Done()
	return res, err
}

var _ = Describe("home checkpoints", func() {
	var (
		e      *env
		home   *fakeHome
		big    string
		writer *checkpoint.Writer
	)
	cps := func(ctx context.Context, extra ...func(*agentd.Checkpoints)) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.Checkpoints = &agentd.Checkpoints{
				Store: writer, Repositories: fixedRepo{}, Interval: 24 * time.Hour, Poll: 50 * time.Millisecond,
				Attempts: 2, Retry: 10 * time.Millisecond, RestoreTries: 1, RestoreFallbacks: 2, Timeout: racescale.D(20 * time.Second),
			}
			for _, x := range extra {
				x(c.Checkpoints)
			}
		}
	}
	checkpointOf := func(ctx context.Context, id pgtype.UUID) func() store.HomeCheckpoint {
		return func() store.HomeCheckpoint {
			cp, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: e.org, ID: id})
			Expect(err).NotTo(HaveOccurred())
			return cp
		}
	}
	status := func(f func() store.HomeCheckpoint) func() store.CheckpointStatus {
		return func() store.CheckpointStatus { return f().Status }
	}
	request := func(ctx context.Context, reason store.CheckpointReason) pgtype.UUID {
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		cp, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
			OrgID: e.org, AgentID: e.agent.ID, Reason: reason, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String,
		})
		Expect(err).NotTo(HaveOccurred())
		return cp.ID
	}
	restoreOf := func(ctx context.Context) pgtype.UUID {
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb.RestoreCheckpointID
	}
	// reconcilerRecreates is what koinon-reconciler does once the backup is complete: it points the
	// next sandbox's restore at the checkpoint, deletes the sandbox, and records the new one.
	generationOfRow := func(ctx context.Context) string {
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb.HomeGeneration.String
	}
	reconcilerRecreates := func(ctx context.Context, cp pgtype.UUID) {
		_, err := q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: e.org, AgentID: e.agent.ID, CheckpointID: cp})
		Expect(err).NotTo(HaveOccurred())
		e.recreateSandbox(ctx)
		home.set(nil) // OpenShell deleted the volume with the sandbox
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, harness.OpenCode)
		// More than the supervisor's 4 MiB exec stdin cap, and incompressible.
		raw := make([]byte, 5<<20+123)
		_, _ = rand.Read(raw)
		big = string(raw)
		home = &fakeHome{files: map[string]string{
			"big/random.bin":                   big,
			"notes/today.md":                   "remember the milk",
			".local/share/opencode/ses_1.db":   "session bytes",
			"work/app/node_modules/x/index.js": "excluded in the sandbox's tar, not in this fake",
			"lost+found/orphan":                "the volume's fsck directory",
			"work/lost+found/mine":             "a directory of the user's",
		}}
		e.files.tar = home.tar
		e.gw.HandleUpload(home.upload)
		writer = &checkpoint.Writer{Storage: checkpoint.Dir(GinkgoT().TempDir()), Scratch: GinkgoT().TempDir()}
	})

	It("backs up a ready sandbox's home every interval", func(ctx SpecContext) {
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Interval = 24 * time.Hour }))
		var cp store.HomeCheckpoint
		Eventually(func(g Gomega) {
			var err error
			cp, err = q.LatestHomeCheckpointOfSandbox(ctx, store.LatestHomeCheckpointOfSandboxParams{
				OrgID: e.org, AgentID: e.agent.ID, SandboxID: e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId(),
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cp.Status).To(Equal(store.CheckpointStatusComplete))
		}).WithTimeout(racescale.D(20 * time.Second)).Should(Succeed())
		Expect(cp.Reason).To(Equal(store.CheckpointReasonHourly))
		Expect(cp.SnapshotID.String).NotTo(BeEmpty())
		Expect(cp.Generation).To(Equal(e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId()))

		By("the snapshot holds the home, and the repository password was made for the agent")
		var out bytes.Buffer
		Expect(writer.Restore(ctx, checkpoint.Prefix(e.org.String(), e.agent.ID.String()), "koinon_test_not-a-real-token", cp.SnapshotID.String, &out)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("remember the milk"))

		By("no second request within the interval")
		time.Sleep(500 * time.Millisecond)
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM home_checkpoints WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("writes no snapshot in restore-only mode, though a checkpoint is requested (ADR 0110 decision 4)", func(ctx SpecContext) {
		e.pool("agentd-ro", cps(ctx, func(c *agentd.Checkpoints) { c.Interval = time.Second; c.ReadOnly = true }))
		cp := request(ctx, store.CheckpointReasonStop)
		Eventually(status(checkpointOf(ctx, cp)), racescale.D(10*time.Second), 100*time.Millisecond).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, cp)().Error.String).To(Equal(checkpoint.ReadOnlyError), "failed at once with the reason, not after SandboxWait")
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM home_checkpoints WHERE org_id = $1 AND status = 'complete'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("resumes the session with session/load in a sandbox recreated from a checkpoint", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		stop := e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		before, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		generation := before.HarnessSessionGeneration.String
		Expect(generation).NotTo(BeEmpty())

		By("the reconciler asks for a backup before the image bump; the writer takes it")
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-b", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))

		By("the sandbox is recreated: an empty home, the same generation")
		reconcilerRecreates(ctx, id)
		Expect(home.get()).To(BeEmpty())
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.HomeGeneration.String).To(Equal(generation))
		Expect(sb.SandboxID.String).NotTo(Equal(generation))

		By("the next run restores the home before the harness starts and loads the session")
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/today.md", "remember the milk"))
		Expect(home.get()).To(HaveKey(".local/share/opencode/ses_1.db"))
		Expect(home.get()["big/random.bin"] == big).To(BeTrue(), "the 5 MiB file came back byte for byte")
		By("the home went through the upload channel, into /sandbox, as an archive that tar can extract as the sandbox user")
		Expect(e.gw.UploadStats().Uploads).To(Equal(1))
		Expect(e.gw.UploadStats().Violations).To(BeZero())
		Expect(e.gw.UploadStats().Bytes).To(BeNumerically(">", 5<<20))
		Expect(e.gw.UploadStats().MaxOutstanding).To(BeNumerically("<=", fakegateway.UploadWindow))
		Expect(home.dests).To(Equal([]string{checkpoint.HomeDir}))
		Expect(home.names).NotTo(BeEmpty())
		for _, n := range home.names {
			Expect(n).NotTo(Or(Equal("."), Equal("./"), HavePrefix("./"), HavePrefix("/")), "no root entry for tar to chmod")
			Expect(checkpoint.Excluded(n)).To(BeFalse(), n)
		}
		Expect(home.get()).NotTo(HaveKey("lost+found/orphan"))
		Expect(home.get()).To(HaveKeyWithValue("work/lost+found/mine", "a directory of the user's"))
		Expect(e.gw.ActiveSSHSessions()).To(BeZero(), "the session token is revoked")
		prompts, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(Equal([]string{"ses_1"}), "session/load, not a reset")
		Expect(prompts).To(Equal([]string{"Task: " + t.Title + "\n\nfirst", "second"}))
		evs := events(ctx, r2)
		session := slices.IndexFunc(evs, func(ev store.RunEvent) bool { return ev.Kind == "run.session" })
		Expect(session).To(BeNumerically(">=", 0))
		Expect(payload(evs[session])).To(HaveKeyWithValue("loaded", true))
		Expect(kinds(evs)[:2]).To(Equal([]string{"run.home_restored", "run.session"}), "restored before the harness started")
		Expect(kindsOf(taskEvents(ctx, e, t))).NotTo(ContainElement("run.session_reset"))
		Expect(kinds(evs)).To(ContainElement("run.home_restored"))
		Expect(restoreOf(ctx).Valid).To(BeFalse(), "the restore is done")
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionGeneration.String).To(Equal(generation))
	})

	It("restores an archived agent's home on its next run and loads the same session (plan item AR1)", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		stop := e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		before, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		generation := before.HarnessSessionGeneration.String

		By("the sandbox stops; its stop backup completes")
		id := request(ctx, store.CheckpointReasonStop)
		e.pool("agentd-b", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))

		By("the reconciler archives it: restore pointed at the backup, sandbox and volume deleted, row deleting/Archived")
		_, err = q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: e.org, AgentID: e.agent.ID, CheckpointID: id})
		Expect(err).NotTo(HaveOccurred())
		_, derr := e.backend.DeleteSandbox(ctx, e.workspace, e.sandbox)
		Expect(derr).To(Succeed())
		home.set(nil) // OpenShell deleted the volume with the sandbox
		_, err = q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
			OrgID: e.org, AgentID: e.agent.ID, State: store.SandboxStateDeleting, Phase: "Archived",
			Message: "archived, restores on next run", HomeGeneration: generationOfRow(ctx),
		})
		Expect(err).NotTo(HaveOccurred())
		archived, err := q.MarkSandboxArchived(ctx, store.MarkSandboxArchivedParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(archived.State).To(Equal(store.SandboxStateDeleting))
		Expect(archived.ArchivedAt.Valid).To(BeTrue())

		By("the next run waits while no sandbox exists, restoring nothing yet")
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t) // agentd-b takes it: a pool without Checkpoints would skip the restore
		Consistently(runState(ctx, r2)).WithTimeout(1500 * time.Millisecond).ShouldNot(Or(Equal(store.RunStateSucceeded), Equal(store.RunStateFailed)))

		By("the reconciler creates the sandbox for the run and records it ready: the archive ends, the restore is still owed")
		_, err = e.backend.CreateSandbox(ctx, e.workspace, execution.SandboxSpec{Name: e.sandbox, Image: "koinon/sandbox:dev"})
		Expect(err).NotTo(HaveOccurred())
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Expect(restoreOf(ctx).Valid).To(BeTrue(), "the restore is owed until the run does it")

		By("the run restores before the harness starts and reloads the session, not a reset")
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(restoreOf(ctx).Valid).To(BeFalse(), "the restore is done")
		sbRow, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sbRow.ArchivedAt.Valid).To(BeFalse(), "archived_at is NULL once the sandbox is observed again")
		Expect(home.get()).To(HaveKeyWithValue("notes/today.md", "remember the milk"))
		_, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(Equal([]string{"ses_1"}), "session/load")
		evs := events(ctx, r2)
		Expect(kinds(evs)[:2]).To(Equal([]string{"run.home_restored", "run.session"}))
		Expect(kindsOf(taskEvents(ctx, e, t))).NotTo(ContainElement("run.session_reset"))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionGeneration.String).To(Equal(generation))
	})

	It("fails a backup that cannot be taken, after its attempts, and leaves the sandbox alone", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, backups: errors.New("S3: access denied")}
		}))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		cp := checkpointOf(ctx, id)()
		Expect(cp.Attempts).To(BeEquivalentTo(2))
		Expect(cp.Error.String).To(ContainSubstring("access denied"))
		Expect(cp.SnapshotID.Valid).To(BeFalse())
	})

	It("bounds a hang before the exec by the backup timeout (#235)", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) {
			c.Repositories = hangingRepo{}
			c.Timeout = 500 * time.Millisecond
			c.Attempts = 1
		}))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, id)().Error.String).To(ContainSubstring("deadline exceeded"))
	})

	It("records a snapshot that finished after the attempt deadline as complete (#235)", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonStop)
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = lateStore{HomeStore: writer}
			// The deadline only has to outlast the database lookups and the in-memory read of the
			// stream: lateStore runs the repository work detached from it and holds the result
			// until the deadline, so the spec lasts as long as Timeout. Not racescaled (#576).
			c.Timeout = 10 * time.Second
			c.Attempts = 1
		}))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(30 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
		Expect(checkpointOf(ctx, id)().SnapshotID.Valid).To(BeTrue())
	})

	It("fails a backup into a prefix that holds a repository the database has no password for, at once, and records the next free epoch (#262)", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		// The database was restored from a dump, then keys forget started epoch 1 again: r1/, r2/ and
		// r4/ hold repositories started after the dump, under passwords only the lost database had;
		// its sweeper deleted r3/ (a gap the search for a free epoch must not stop at).
		foreign := map[int32]checkpoint.Result{}
		for _, n := range []int32{1, 2, 4} {
			res, err := writer.Backup(ctx, checkpoint.EpochPrefix(org, agent, n), fmt.Sprintf("koinon_test_foreign-%d", n),
				tarOf(map[string]string{"notes/v.md": "after the dump"}), checkpoint.Meta{CheckpointID: "x"})
			Expect(err).NotTo(HaveOccurred())
			foreign[n] = res
		}
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, retired_at, delete_after)
			VALUES ($1, $2, $3, '{}', now(), now() + interval '7 days')`, e.org, e.agent.ID, checkpoint.Prefix(org, agent))
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 1, $3, '{}', 'key_lost')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 1))
		Expect(err).NotTo(HaveOccurred())
		current := &atomic.Int32{}
		current.Store(1)

		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Repositories = fixedRepo{current: current} }))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		cp := checkpointOf(ctx, id)()
		Expect(cp.Attempts).To(BeEquivalentTo(1), "another attempt cannot help")
		Expect(cp.Error.String).To(And(ContainSubstring(checkpoint.EpochPrefix(org, agent, 1)),
			ContainSubstring("koinon-api keys new-repositories --agent "+agent), ContainSubstring("free epoch 5")))

		By("the free epoch is the floor of the next one, and the taken row is marked, once")
		var floor pgtype.Int4
		var taken0 bool
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least, prefix_taken_at IS NOT NULL FROM home_repositories WHERE org_id = $1 AND agent_id = $2`,
			e.org, e.agent.ID).Scan(&floor, &taken0)).To(Succeed())
		Expect(floor).To(Equal(pgtype.Int4{Int32: 5, Valid: true}), "past r4/, not at the gap r3/")
		Expect(taken0).To(BeFalse(), "epoch 0's prefix is the agent's own")
		var taken1 bool
		Expect(db.QueryRow(ctx, `SELECT prefix_taken_at IS NOT NULL FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`,
			e.org, e.agent.ID).Scan(&taken1)).To(Succeed())
		Expect(taken1).To(BeTrue())
		var events []string
		rows, err := db.Query(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, agentd.PrefixTakenAction)
		Expect(err).NotTo(HaveOccurred())
		events, err = pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(ConsistOf(And(ContainSubstring(`"repositoryEpoch": 1`), ContainSubstring(`"nextEpochAtLeast": 5`))))

		By("nothing was written over the repository found")
		for n, res := range foreign {
			snaps, err := writer.Snapshots(ctx, checkpoint.EpochPrefix(org, agent, n), fmt.Sprintf("koinon_test_foreign-%d", n))
			Expect(err).NotTo(HaveOccurred())
			Expect(snaps).To(HaveLen(1))
			Expect(snaps[0].ID).To(Equal(res.SnapshotID))
		}
	})

	It("fails a backup into a repository that does not open with the password that wrote its checkpoints, and records no taken prefix (#262)", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		// The database records a complete checkpoint in epoch 0, but the prefix holds a repository
		// under another password: koinon-agentd points at another install's bucket, or kopia.repository
		// is damaged. A new epoch would retire this one, and the sweeper would delete its homes.
		_, err := writer.Backup(ctx, checkpoint.Prefix(org, agent), "koinon_test_other-bucket",
			tarOf(map[string]string{"notes/v.md": "elsewhere"}), checkpoint.Meta{CheckpointID: "x"})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
			e.org, e.agent.ID, checkpoint.Prefix(org, agent))
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', 's0', now() - interval '1 hour', 0)`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())

		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		cp := checkpointOf(ctx, id)()
		Expect(cp.Attempts).To(BeEquivalentTo(1))
		Expect(cp.Error.String).To(And(ContainSubstring(checkpoint.Prefix(org, agent)),
			ContainSubstring("does not open with the password that wrote its 1 complete checkpoint(s)"), ContainSubstring("wrong bucket")))
		Expect(cp.Error.String).NotTo(ContainSubstring("keys new-repositories --agent"), "a new epoch would retire the homes it holds")

		var floor pgtype.Int4
		var taken bool
		Expect(db.QueryRow(ctx, `SELECT next_epoch_at_least, prefix_taken_at IS NOT NULL FROM home_repositories WHERE org_id = $1 AND agent_id = $2`,
			e.org, e.agent.ID).Scan(&floor, &taken)).To(Succeed())
		Expect(floor.Valid).To(BeFalse())
		Expect(taken).To(BeFalse(), "the sweeper treats it as the row's own")
		var audits int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2`, e.org, agentd.PrefixTakenAction).Scan(&audits)).To(Succeed())
		Expect(audits).To(BeZero())
	})

	It("marks a retired repository whose prefix was taken deleted without deleting the blobs there (#262)", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		res, err := writer.Backup(ctx, checkpoint.EpochPrefix(org, agent, 1), "koinon_test_foreign-1",
			tarOf(map[string]string{"notes/v.md": "after the dump"}), checkpoint.Meta{CheckpointID: "x"})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, retired_at, delete_after, deleted_at)
			VALUES ($1, $2, $3, '{}', now(), now(), now())`, e.org, e.agent.ID, checkpoint.Prefix(org, agent))
		Expect(err).NotTo(HaveOccurred())
		// Epoch 1 was found taken and replaced by epoch 3 (keys new-repositories), which holds the home.
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, prefix_taken_at, retired_at, delete_after)
			VALUES ($1, $2, 1, $3, '{}', 'key_lost', now(), now(), now() - interval '1 minute')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 1))
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 3, $3, '{}', 'key_compromised')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 3))
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', 's3', now(), 3)`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())

		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Sweep = 50 * time.Millisecond }))
		Eventually(func() bool {
			var deleted bool
			Expect(db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2 AND repository_epoch = 1`,
				e.org, e.agent.ID).Scan(&deleted)).To(Succeed())
			return deleted
		}).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
		snaps, err := writer.Snapshots(ctx, checkpoint.EpochPrefix(org, agent, 1), "koinon_test_foreign-1")
		Expect(err).NotTo(HaveOccurred(), "the repository found there is not the row's: kept")
		Expect(snaps).To(HaveLen(1))
		Expect(snaps[0].ID).To(Equal(res.SnapshotID))
	})

	It("fails a backup when tar in the sandbox fails", func(ctx SpecContext) {
		home.failRead = true
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, id)().Error.String).To(ContainSubstring("Input/output error"))
		By("the home tar did not read whole is not in the bucket either: checkpoints resync records what is there")
		_, err := writer.Snapshots(ctx, checkpoint.Prefix(e.org.String(), e.agent.ID.String()), "koinon_test_not-a-real-token")
		Expect(err).To(MatchError(checkpoint.ErrNoRepositoryInBucket))
	})

	It("does not back up a sandbox that is waiting for its restore", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonStop)
		_, err := q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: e.org, AgentID: e.agent.ID, CheckpointID: id})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, id)().Error.String).To(ContainSubstring("waiting for its home to be restored"))
	})

	It("restores the newest checkpoint that restores, and fails the run when none does", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()

		take := func(files map[string]string) pgtype.UUID {
			home.set(files)
			id := request(ctx, store.CheckpointReasonStop)
			stopW := e.pool("agentd-w", cps(ctx))
			Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
			stopW()
			return id
		}
		older := take(map[string]string{"notes/v.md": "old"})
		newer := take(map[string]string{"notes/v.md": "new"})
		Expect(checkpointOf(ctx, newer)().SnapshotID.String).NotTo(Equal(checkpointOf(ctx, older)().SnapshotID.String))

		reconcilerRecreates(ctx, newer)
		bad := checkpointOf(ctx, newer)().SnapshotID.String
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: bad}
		}))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/v.md", "old"), "the newest would not restore; the older one did")
		Expect(restoreOf(ctx).Valid).To(BeFalse())
	})

	It("writes to the current repository epoch and restores each checkpoint from the one it was written to", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()

		current := &atomic.Int32{}
		epochs := func(c *agentd.Checkpoints) { c.Repositories = fixedRepo{current: current} }
		take := func(files map[string]string) pgtype.UUID {
			home.set(files)
			id := request(ctx, store.CheckpointReasonStop)
			stopW := e.pool("agentd-w", cps(ctx, epochs))
			Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
			stopW()
			return id
		}
		older := take(map[string]string{"notes/v.md": "old"})
		By("a key-encryption key is lost: the agent's repository epoch 1 starts")
		current.Store(1)
		newer := take(map[string]string{"notes/v.md": "new"})
		Expect(checkpointOf(ctx, older)().RepositoryEpoch).To(BeZero())
		Expect(checkpointOf(ctx, newer)().RepositoryEpoch).To(Equal(int32(1)))
		var out bytes.Buffer
		Expect(writer.Restore(ctx, checkpoint.EpochPrefix(e.org.String(), e.agent.ID.String(), 1), "koinon_test_not-a-real-token-1",
			checkpointOf(ctx, newer)().SnapshotID.String, &out)).To(Succeed(), "written to epoch 1's repository, with its password")

		By("the newest checkpoint restores from epoch 1")
		reconcilerRecreates(ctx, newer)
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		stopB := e.pool("agentd-b", cps(ctx, epochs))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stopB()
		Expect(home.get()).To(HaveKeyWithValue("notes/v.md", "new"))

		By("when it will not restore, the older one restores from epoch 0")
		reconcilerRecreates(ctx, newer)
		e.message(ctx, t, "third")
		r3 := e.enqueue(ctx, t)
		e.pool("agentd-c", cps(ctx, epochs, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: checkpointOf(ctx, newer)().SnapshotID.String}
		}))
		Eventually(runState(ctx, r3)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/v.md", "old"))
	})

	It("skips a checkpoint whose repository password was lost and restores an older one", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		current := &atomic.Int32{}
		take := func(files map[string]string) pgtype.UUID {
			home.set(files)
			id := request(ctx, store.CheckpointReasonStop)
			stopW := e.pool("agentd-w", cps(ctx, func(c *agentd.Checkpoints) { c.Repositories = fixedRepo{current: current} }))
			Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
			stopW()
			return id
		}
		take(map[string]string{"notes/v.md": "old"})
		current.Store(1)
		newer := take(map[string]string{"notes/v.md": "new"})

		reconcilerRecreates(ctx, newer)
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) {
			c.Repositories = fixedRepo{current: current, lost: map[int32]bool{1: true}}
		}))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/v.md", "old"), "epoch 1's password is lost; epoch 0's checkpoint restored")
	})

	It("deletes a retired repository past its deadline once a later epoch holds a checkpoint, and no longer restores its own", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		e0 := checkpoint.Prefix(org, agent)
		e1 := checkpoint.EpochPrefix(org, agent, 1)
		_, err := writer.Backup(ctx, e0, "koinon_test_not-a-real-token", tarOf(map[string]string{"notes/v.md": "swept"}), checkpoint.Meta{CheckpointID: "c0"})
		Expect(err).NotTo(HaveOccurred())
		s1, err := writer.Backup(ctx, e1, "koinon_test_not-a-real-token-1", tarOf(map[string]string{"notes/v.md": "swept"}), checkpoint.Meta{CheckpointID: "c1"})
		Expect(err).NotTo(HaveOccurred())
		// The rows as NewEpoch leaves them (the envelopes are not opened here), epoch 0 due now.
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, retired_at, delete_after)
			VALUES ($1, $2, $3, '{}', now() - interval '8 days', now() - interval '1 minute')`, e.org, e.agent.ID, e0)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 1, $3, '{}', 'key_compromised')`, e.org, e.agent.ID, e1)
		Expect(err).NotTo(HaveOccurred())
		complete := func(epoch int, snapshot string) pgtype.UUID {
			GinkgoHelper()
			// Inserted complete, so the running pool has no request to claim.
			var id pgtype.UUID
			Expect(db.QueryRow(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
				VALUES ($1, $2, 'stop', 'complete', $3, now(), $4) RETURNING id`, e.org, e.agent.ID, snapshot, epoch).Scan(&id)).To(Succeed())
			return id
		}
		old := complete(0, "gone")
		listed := func() int {
			cps, err := q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{OrgID: e.org, AgentID: e.agent.ID, MaxRows: 10})
			Expect(err).NotTo(HaveOccurred())
			return len(cps)
		}
		deleted0 := func() bool {
			var deleted pgtype.Timestamptz
			Expect(db.QueryRow(ctx, `SELECT deleted_at FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&deleted)).To(Succeed())
			return deleted.Valid
		}
		Expect(listed()).To(Equal(1))

		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Sweep = 50 * time.Millisecond }))
		By("past its deadline, but the agent's newest restorable home: kept")
		Consistently(deleted0).WithTimeout(time.Second).Should(BeFalse())
		var out bytes.Buffer
		Expect(writer.Restore(ctx, e0, "koinon_test_not-a-real-token", "", &out)).To(Succeed(), "epoch 0 still restores")

		By("epoch 1's first checkpoint completes: epoch 0 goes")
		complete(1, s1.SnapshotID)
		Expect(listed()).To(Equal(2))
		Eventually(deleted0).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
		Expect(listed()).To(Equal(1), "a deleted repository's checkpoint is not offered to a restore")

		out.Reset()
		Expect(writer.Restore(ctx, e0, "koinon_test_not-a-real-token", "", &out)).NotTo(Succeed(), "epoch 0's blobs are deleted")
		out.Reset()
		Expect(writer.Restore(ctx, e1, "koinon_test_not-a-real-token-1", s1.SnapshotID, &out)).To(Succeed(), "epoch 1 under it is untouched")
		var deleted1 pgtype.Timestamptz
		Expect(db.QueryRow(ctx, `SELECT deleted_at FROM home_repository_epochs WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&deleted1)).To(Succeed())
		Expect(deleted1.Valid).To(BeFalse(), "the current epoch is not retired, so not swept")

		By("a checkpoint of a later epoch, once that epoch is deleted, is not offered either")
		_, err = db.Exec(ctx, `UPDATE home_checkpoints SET repository_epoch = 1 WHERE org_id = $1 AND id = $2`, e.org, old)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).To(Equal(2))
		_, err = db.Exec(ctx, `UPDATE home_repository_epochs SET retired_at = now(), delete_after = now(), deleted_at = now() WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed()).To(BeZero())
	})

	It("deletes a retired repository whose password was lost without waiting for a later checkpoint, and never one whose stored prefix is not its own", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		e0 := checkpoint.Prefix(org, agent)
		_, err := writer.Backup(ctx, e0, "koinon_test_not-a-real-token", tarOf(map[string]string{"notes/v.md": "lost"}), checkpoint.Meta{CheckpointID: "c0"})
		Expect(err).NotTo(HaveOccurred())
		// Another agent's repository, which a tampered row of this agent names.
		other := checkpoint.Prefix(org, "0198a000-0000-7000-8000-0000000000aa")
		_, err = writer.Backup(ctx, other, "koinon_test_not-a-real-token-other", tarOf(map[string]string{"notes/v.md": "theirs"}), checkpoint.Meta{CheckpointID: "x"})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, credential_lost_at, retired_at, delete_after)
			VALUES ($1, $2, $3, NULL, now(), now() - interval '8 days', now() - interval '1 minute')`, e.org, e.agent.ID, other)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 1, $3, '{}', 'key_lost')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 1))
		Expect(err).NotTo(HaveOccurred())
		deleted0 := func() bool {
			var deleted pgtype.Timestamptz
			Expect(db.QueryRow(ctx, `SELECT deleted_at FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&deleted)).To(Succeed())
			return deleted.Valid
		}

		// Inserted complete, so the running pool has no hourly request to claim and back up into e0 beside the sweep.
		_, err = db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
			VALUES ($1, $2, 'stop', 'complete', 'lost', now(), 0)`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())

		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Sweep = 50 * time.Millisecond }))
		By("the stored prefix names another agent's folder: refused, nothing deleted")
		Consistently(deleted0).WithTimeout(time.Second).Should(BeFalse())
		var out bytes.Buffer
		Expect(writer.Restore(ctx, other, "koinon_test_not-a-real-token-other", "", &out)).To(Succeed(), "the other agent's repository is untouched")

		By("with its own prefix, a lost repository goes although no later epoch has a checkpoint")
		_, err = db.Exec(ctx, `UPDATE home_repositories SET prefix = $3 WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID, e0)
		Expect(err).NotTo(HaveOccurred())
		Eventually(deleted0).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
		out.Reset()
		Expect(writer.Restore(ctx, e0, "koinon_test_not-a-real-token", "", &out)).NotTo(Succeed(), "its blobs are deleted")
	})

	It("counts a refused repository in a gauge on every sweep and logs it once", func(ctx SpecContext) {
		org := e.org.String()
		other := checkpoint.Prefix(org, "0198a000-0000-7000-8000-0000000000bb")
		_, err := db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, credential_lost_at, retired_at, delete_after)
			VALUES ($1, $2, $3, NULL, now(), now() - interval '8 days', now() - interval '1 minute')`, e.org, e.agent.ID, other)
		Expect(err).NotTo(HaveOccurred())
		m := agentd.NewMetrics(nil)
		logs := &syncBuffer{}
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Sweep = 50 * time.Millisecond }), func(c *agentd.Config) {
			c.Metrics = m
			c.Logger = slog.New(slog.NewTextHandler(io.MultiWriter(GinkgoWriter, logs), nil))
		})
		Eventually(func() float64 { return testutil.ToFloat64(m.SweepRefused) }).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(1.0))
		Consistently(func() float64 { return testutil.ToFloat64(m.SweepRefused) }).WithTimeout(500*time.Millisecond).Should(Equal(1.0), "every sweep counts it again")
		Expect(strings.Count(logs.String(), "stored prefix is not its own")).To(Equal(1), "about ten sweeps, one log line")

		By("once the row is fixed, the gauge goes back to 0 (and the row is swept)")
		_, err = db.Exec(ctx, `UPDATE home_repositories SET prefix = $3 WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID, checkpoint.Prefix(org, e.agent.ID.String()))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() float64 { return testutil.ToFloat64(m.SweepRefused) }).WithTimeout(racescale.D(10 * time.Second)).Should(BeZero())

		By("a fixed row that regresses is logged again")
		_, err = db.Exec(ctx, `UPDATE home_repositories SET prefix = $3, deleted_at = NULL WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID, other)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { return strings.Count(logs.String(), "stored prefix is not its own") }).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(2))
	})

	It("keeps a retired repository while the only later checkpoints are in a lost or deleted epoch", func(ctx SpecContext) {
		org, agent := e.org.String(), e.agent.ID.String()
		e0 := checkpoint.Prefix(org, agent)
		_, err := writer.Backup(ctx, e0, "koinon_test_not-a-real-token", tarOf(map[string]string{"notes/v.md": "kept"}), checkpoint.Meta{CheckpointID: "c0"})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope, retired_at, delete_after)
			VALUES ($1, $2, $3, '{}', now() - interval '8 days', now() - interval '1 minute')`, e.org, e.agent.ID, e0)
		Expect(err).NotTo(HaveOccurred())
		// Epoch 1 lost its password (not yet due itself); epoch 2 is deleted already.
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, credential_lost_at, reason, retired_at, delete_after)
			VALUES ($1, $2, 1, $3, NULL, now(), 'key_lost', now(), now() + interval '7 days')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 1))
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason, retired_at, delete_after, deleted_at)
			VALUES ($1, $2, 2, $3, '{}', 'key_lost', now(), now(), now())`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 2))
		Expect(err).NotTo(HaveOccurred())
		complete := func(epoch int) {
			GinkgoHelper()
			_, err := db.Exec(ctx, `INSERT INTO home_checkpoints (org_id, agent_id, reason, status, snapshot_id, finished_at, repository_epoch)
				VALUES ($1, $2, 'stop', 'complete', $3, now(), $4)`, e.org, e.agent.ID, fmt.Sprintf("s%d", epoch), epoch)
			Expect(err).NotTo(HaveOccurred())
		}
		complete(1)
		complete(2)
		deleted0 := func() bool {
			var deleted pgtype.Timestamptz
			Expect(db.QueryRow(ctx, `SELECT deleted_at FROM home_repositories WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID).Scan(&deleted)).To(Succeed())
			return deleted.Valid
		}

		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) { c.Sweep = 50 * time.Millisecond }))
		Consistently(deleted0).WithTimeout(time.Second).Should(BeFalse(), "neither later checkpoint can be restored")

		By("a checkpoint of a later epoch that still opens releases it")
		_, err = db.Exec(ctx, `INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
			VALUES ($1, $2, 3, $3, '{}', 'key_lost')`, e.org, e.agent.ID, checkpoint.EpochPrefix(org, agent, 3))
		Expect(err).NotTo(HaveOccurred())
		complete(3)
		Eventually(deleted0).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
	})

	It("does not start the harness when the home cannot be restored", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		id := request(ctx, store.CheckpointReasonRecreate)
		stopW := e.pool("agentd-w", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
		stopW()
		reconcilerRecreates(ctx, id)
		prompts0, _, _, _ := e.harness.snapshot()

		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		bad := checkpointOf(ctx, id)().SnapshotID.String
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: bad}
		}))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal(prompts0), "the harness never saw a prompt")
		Expect(getRun(ctx, r2).OutcomeReason.String).To(ContainSubstring("restoring the home failed"))
		Expect(restoreOf(ctx).Valid).To(BeTrue(), "the restore is still owed")
	})

	It("fails the run with the OpenShell version it needs when the sandbox's supervisor has no upload channel", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		id := request(ctx, store.CheckpointReasonRecreate)
		stopW := e.pool("agentd-w", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
		stopW()
		reconcilerRecreates(ctx, id)

		e.gw.DisableUploadSubsystem()
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) { c.RestoreTries = 3 }))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		reason := getRun(ctx, r2).OutcomeReason.String
		Expect(reason).To(ContainSubstring("does not support openshell-upload-v1"))
		Expect(reason).To(ContainSubstring("OpenShell v0.1.4 or later"))
		Expect(e.gw.Calls("CreateSshSession")).To(Equal(1), "no retry and no older checkpoint: they would fail the same way")
		Expect(restoreOf(ctx).Valid).To(BeTrue(), "the restore is still owed")
		Expect(e.gw.ActiveSSHSessions()).To(BeZero())
	})

	It("stops staging the snapshot when the upload cannot start", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		id := request(ctx, store.CheckpointReasonRecreate)
		stopW := e.pool("agentd-w", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.CheckpointStatusComplete))
		stopW()
		reconcilerRecreates(ctx, id)

		e.gw.DisableUploadSubsystem()
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		stopped := make(chan struct{})
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = blockingStore{HomeStore: writer, stopped: stopped}
			c.Timeout = 10 * time.Minute // far longer than the spec waits
		}))
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r2).OutcomeReason.String).To(ContainSubstring("openshell-upload-v1"))
		Eventually(stopped).WithTimeout(racescale.D(5*time.Second)).Should(BeClosed(), "the writer was cancelled, not waited for")
	})
})

// syncBuffer is a bytes.Buffer the pool's goroutines can log to while a spec reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
