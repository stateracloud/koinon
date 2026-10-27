// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// fakeHome is the sandbox's home as the fake tar sees it: paths under /sandbox and their bytes.
type fakeHome struct {
	mu    sync.Mutex
	files map[string]string
	// failRead makes the backup's tar fail with exit code 2.
	failRead bool
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

// tar answers `tar -C /sandbox ... -cf - .` (the backup) and `tar -C /sandbox ... -xf -` (the
// restore).
func (h *fakeHome) tar(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case slices.Contains(args, "-cf"):
		if h.failRead {
			_, _ = io.WriteString(stderr, "tar: /sandbox: Input/output error")
			return 2
		}
		tw := tar.NewWriter(stdout)
		_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
		for name, body := range h.files {
			_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			_, _ = io.WriteString(tw, body)
		}
		_ = tw.Close()
		return 0
	case slices.Contains(args, "-xf"):
		if h.files == nil {
			h.files = map[string]string{}
		}
		tr := tar.NewReader(stdin)
		for {
			hd, err := tr.Next()
			if err != nil {
				return 0
			}
			if hd.Typeflag != tar.TypeReg {
				continue
			}
			b, _ := io.ReadAll(tr)
			h.files[strings.TrimPrefix(hd.Name, "./")] = string(b)
		}
	}
	_, _ = io.WriteString(stderr, "tar: unexpected arguments")
	return 2
}

type fixedRepo struct{}

func (fixedRepo) Get(_ context.Context, org, agent pgtype.UUID) (checkpoint.Repository, error) {
	return checkpoint.Repository{Prefix: checkpoint.Prefix(org.String(), agent.String()), Password: "koinon_test_not-a-real-token"}, nil
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

var _ = Describe("home checkpoints", func() {
	var (
		e      *env
		home   *fakeHome
		writer *checkpoint.Writer
	)
	cps := func(ctx context.Context, extra ...func(*agentd.Checkpoints)) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.Checkpoints = &agentd.Checkpoints{
				Store: writer, Repositories: fixedRepo{}, Interval: 24 * time.Hour, Poll: 50 * time.Millisecond,
				Attempts: 2, Retry: 10 * time.Millisecond, RestoreTries: 1, RestoreFallbacks: 2, Timeout: 20 * time.Second,
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
	reconcilerRecreates := func(ctx context.Context, cp pgtype.UUID) {
		_, err := q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: e.org, AgentID: e.agent.ID, CheckpointID: cp})
		Expect(err).NotTo(HaveOccurred())
		e.recreateSandbox(ctx)
		home.set(nil) // OpenShell deleted the volume with the sandbox
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, store.AgentFlavorOpencode)
		home = &fakeHome{files: map[string]string{
			"notes/today.md":                   "remember the milk",
			".local/share/opencode/ses_1.db":   "session bytes",
			"work/app/node_modules/x/index.js": "excluded in the sandbox's tar, not in this fake",
		}}
		e.files.tar = home.tar
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
		}).WithTimeout(20 * time.Second).Should(Succeed())
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

	It("resumes the session with session/load in a sandbox recreated from a checkpoint", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		stop := e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		stop()
		before, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		generation := before.HarnessSessionGeneration.String
		Expect(generation).NotTo(BeEmpty())

		By("the reconciler asks for a backup before the image bump; the writer takes it")
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-b", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusComplete))

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
		Eventually(runState(ctx, r2)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/today.md", "remember the milk"))
		Expect(home.get()).To(HaveKey(".local/share/opencode/ses_1.db"))
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

	It("fails a backup that cannot be taken, after its attempts, and leaves the sandbox alone", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, backups: errors.New("S3: access denied")}
		}))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusFailed))
		cp := checkpointOf(ctx, id)()
		Expect(cp.Attempts).To(BeEquivalentTo(2))
		Expect(cp.Error.String).To(ContainSubstring("access denied"))
		Expect(cp.SnapshotID.Valid).To(BeFalse())
	})

	It("fails a backup when tar in the sandbox fails", func(ctx SpecContext) {
		home.failRead = true
		id := request(ctx, store.CheckpointReasonRecreate)
		e.pool("agentd-a", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, id)().Error.String).To(ContainSubstring("Input/output error"))
	})

	It("does not back up a sandbox that is waiting for its restore", func(ctx SpecContext) {
		id := request(ctx, store.CheckpointReasonStop)
		_, err := q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: e.org, AgentID: e.agent.ID, CheckpointID: id})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusFailed))
		Expect(checkpointOf(ctx, id)().Error.String).To(ContainSubstring("waiting for its home to be restored"))
	})

	It("restores the newest checkpoint that restores, and fails the run when none does", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		stop()

		take := func(files map[string]string) pgtype.UUID {
			home.set(files)
			id := request(ctx, store.CheckpointReasonStop)
			stopW := e.pool("agentd-w", cps(ctx))
			Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusComplete))
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
		Eventually(runState(ctx, r2)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(home.get()).To(HaveKeyWithValue("notes/v.md", "old"), "the newest would not restore; the older one did")
		Expect(restoreOf(ctx).Valid).To(BeFalse())
	})

	It("does not start the harness when the home cannot be restored", func(ctx SpecContext) {
		t := e.task(ctx, "first")
		stop := e.pool("agentd-a")
		r1 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		stop()
		id := request(ctx, store.CheckpointReasonRecreate)
		stopW := e.pool("agentd-w", cps(ctx))
		Eventually(status(checkpointOf(ctx, id))).WithTimeout(20 * time.Second).Should(Equal(store.CheckpointStatusComplete))
		stopW()
		reconcilerRecreates(ctx, id)
		prompts0, _, _, _ := e.harness.snapshot()

		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		bad := checkpointOf(ctx, id)().SnapshotID.String
		e.pool("agentd-b", cps(ctx, func(c *agentd.Checkpoints) {
			c.Store = failingStore{HomeStore: writer, failRestore: bad}
		}))
		Eventually(runState(ctx, r2)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateFailed))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal(prompts0), "the harness never saw a prompt")
		Expect(getRun(ctx, r2).OutcomeReason.String).To(ContainSubstring("restoring the home failed"))
		Expect(restoreOf(ctx).Valid).To(BeTrue(), "the restore is still owed")
	})
})
