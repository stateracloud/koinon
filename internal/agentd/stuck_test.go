// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

const (
	baseSHA = "0123456789abcdef0123456789abcdef01234567"
	// stuckPID is the made-up process the fake sandbox cannot end.
	stuckPID = "41:98765"
)

// fakeKeyring is a keyring with an obviously fake key.
func fakeKeyring() *secretstore.Keyring {
	k, err := secretstore.NewKeyring(map[string][]byte{"kek-test": bytes.Repeat([]byte("k"), secretstore.KeySize)}, "kek-test")
	Expect(err).NotTo(HaveOccurred())
	return k
}

// bound is an agent version config that binds the agent to acme/widgets.
func bound(e *env) string {
	return fmt.Sprintf(`{"model":%q,"instructions":%q,"bindings":[{"connector":"github","connectionId":"%s","targets":["acme/widgets"]}]}`,
		model, instructions, e.user.String())
}

// stuckAfterTurn makes the sweep that follows the harness report a process that will not die.
func (e *env) stuckAfterTurn(procinfo string) {
	e.files.sweep = func() (string, int) {
		if e.files.sweeps%2 == 1 { // the sweep before the harness
			return "ended: none\n", 0
		}
		return "survivors: " + stuckPID + "\nstill running: 41\n", 1
	}
	e.files.procinfo = func() string { return procinfo }
}

const dProc = "41|D|worker|nfs_wait_bit_killable\n"

// uploadKey stands in for the Secret that signs uploads; obviously fake.
var uploadKey = bytes.Repeat([]byte("u"), 32)

// receiverURL starts koinon-api's saved-work receiver (which holds the keyring; koinon-agentd does
// not) on the suite's database and returns its address.
func receiverURL() string {
	mux := http.NewServeMux()
	mux.Handle(savedwork.UploadPath, savedwork.NewReceiver(db, fakeKeyring(),
		func(context.Context) ([]byte, error) { return uploadKey, nil }, nil))
	srv := httptest.NewServer(mux)
	DeferCleanup(srv.Close)
	return srv.URL
}

// fastStuck is a pool config with a 400 ms stuck time that posts saved work to url.
func fastStuck(url string) func(*agentd.Config) {
	return func(c *agentd.Config) {
		c.StuckAfter, c.StuckPoll = 400*time.Millisecond, 50*time.Millisecond
		c.SaveTimeout = 2 * time.Second
		c.SavedWorkURL = url
		c.SavedWorkKey = func() ([]byte, error) { return uploadKey, nil }
	}
}

var _ = Describe("A sandbox with a process that survives SIGKILL", func() {
	changes := "R acme/widgets " + baseSHA + "\nF M 100644 6 2 1 0 a.txt\nhello\n\nF D 000000 0 0 4 0 old.txt\nE\n"

	It("marks the sandbox unhealthy after 60 s (here 400 ms), records /proc's state and wchan, and saves the work for an approval", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		e.stuckAfterTurn(dProc)
		e.files.savework = func(dir string) (string, int) {
			Expect(dir).To(Equal("/sandbox/work"))
			return changes, 0
		}
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10*time.Second)).Should(Equal(store.RunStateSucceeded), "the run itself ended well")

		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.UnhealthyAt.Valid).To(BeTrue())
		Expect(sb.UnhealthyReason.String).To(And(ContainSubstring("worker"), ContainSubstring("pid 41"), ContainSubstring("state D"), ContainSubstring("nfs_wait_bit_killable")))
		Expect(sb.UnhealthyGeneration.String).To(Equal(sb.HomeGeneration.String))

		var procs, outcome, work string
		var savedID *string
		Expect(db.QueryRow(ctx, `SELECT processes::text, outcome, work, saved_work_id::text FROM sandbox_incidents WHERE org_id = $1 AND agent_id = $2`,
			e.org, e.agent.ID).Scan(&procs, &outcome, &work, &savedID)).To(Succeed())
		Expect(procs).To(ContainSubstring(`"wchan": "nfs_wait_bit_killable"`))
		Expect(procs).To(ContainSubstring(`"state": "D"`))
		Expect(procs).To(ContainSubstring(`"comm": "worker"`))
		Expect(outcome).To(Equal("recreate"))
		Expect(work).To(Equal("saved"))
		Expect(savedID).NotTo(BeNil())

		// The work is sealed, tied to the task and the run, expiring in 14 days.
		var id, taskID, runID, repo, branch, status string
		var envelope []byte
		var expires time.Time
		Expect(db.QueryRow(ctx, `SELECT id::text, task_id::text, run_id::text, repo, branch, status, envelope::text::bytea, expires_at FROM saved_work WHERE org_id = $1`, e.org).
			Scan(&id, &taskID, &runID, &repo, &branch, &status, &envelope, &expires)).To(Succeed())
		Expect(taskID).To(Equal(t.ID.String()))
		Expect(runID).To(Equal(r.ID.String()))
		Expect(repo).To(Equal("acme/widgets"))
		Expect(branch).To(HavePrefix("koinon/wip/" + strings.ReplaceAll(t.ID.String(), "-", "")[:8] + "-"))
		Expect(status).To(Equal("saved"))
		Expect(expires).To(BeTemporally("~", time.Now().Add(14*24*time.Hour), time.Minute))
		Expect(string(envelope)).NotTo(ContainSubstring("hello"), "the files are not in the clear")
		var env secretstore.Envelope
		Expect(json.Unmarshal(envelope, &env)).To(Succeed())
		w, err := savedwork.Open(fakeKeyring(), e.org.String(), id, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Manifest.Files).To(HaveLen(2))
		Expect(string(w.Content["a.txt"])).To(Equal("hello\n"))

		// A system-made ask-tier request, and nothing for the run to wait for.
		var tool, target, astatus string
		var system bool
		Expect(db.QueryRow(ctx, `SELECT tool, target, status::text, system FROM approval_requests WHERE org_id = $1 AND task_id = $2`, e.org, t.ID).
			Scan(&tool, &target, &astatus, &system)).To(Succeed())
		Expect(tool).To(Equal("push_saved_work"))
		Expect(target).To(Equal("github:acme/widgets"))
		Expect(astatus).To(Equal("pending"))
		Expect(system).To(BeTrue())
		pending, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: e.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty())

		// The timeline says so; the audit log has the marking.
		var recreated string
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "sandbox.recreated" {
				recreated = string(ev.Payload)
			}
		}
		Expect(recreated).To(ContainSubstring("Sandbox recreated: a process could not be ended"))
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElement("approval.requested"))
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'sandbox.mark_unhealthy'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("does nothing when the process ends before the time is up", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		e.stuckAfterTurn("")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(func() int { e.files.mu.Lock(); defer e.files.mu.Unlock(); return e.files.procinfoRuns }).Should(BeNumerically(">=", 1))
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.UnhealthyAt.Valid).To(BeFalse())
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM sandbox_incidents WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("watches the process for the whole time before it decides", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		e.stuckAfterTurn(dProc)
		start := time.Now()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(func() bool {
			sb, _ := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
			return sb.UnhealthyAt.Valid
		}).WithTimeout(racescale.D(5 * time.Second)).Should(BeTrue())
		Expect(time.Since(start)).To(BeNumerically(">=", 400*time.Millisecond))
		e.files.mu.Lock()
		defer e.files.mu.Unlock()
		Expect(e.files.procinfoRuns).To(BeNumerically(">", 2), "it re-reads /proc every few seconds, not once")
	})

	DescribeTable("says in the timeline that the uncommitted work was lost, and why",
		func(ctx SpecContext, save func(*files), mutate func(*agentd.Config), why string) {
			e := newEnvWith(ctx, harness.OpenCode, bound)
			e.stuckAfterTurn(dProc)
			save(e.files)
			t := e.task(ctx, "hello")
			r := e.enqueue(ctx, t)
			e.pool("agentd-a", fastStuck(receiverURL()), mutate)
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Eventually(func() []string { return kindsOf(taskEvents(ctx, e, t)) }).WithTimeout(racescale.D(5 * time.Second)).Should(ContainElement("sandbox.work_lost"))
			var lost string
			for _, ev := range taskEvents(ctx, e, t) {
				if ev.Kind == "sandbox.work_lost" {
					lost = string(ev.Payload)
				}
			}
			Expect(lost).To(ContainSubstring("uncommitted work was lost"))
			Expect(lost).To(ContainSubstring(why))
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM saved_work WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), "nothing was stored")
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE org_id = $1`, e.org).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), "and no approval asked for")
			sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.UnhealthyAt.Valid).To(BeTrue(), "the sandbox is recreated all the same")
		},
		Entry("reading the workspace takes too long", func(f *files) {
			f.savework = func(string) (string, int) { time.Sleep(3 * time.Second); return "", 0 }
		}, func(c *agentd.Config) { c.SaveTimeout = 300 * time.Millisecond }, "took longer than 300ms"),
		Entry("the work is over 50 MB", func(f *files) {
			f.savework = func(string) (string, int) { return "", 3 }
		}, func(*agentd.Config) {}, "larger than 50 MB"),
		Entry("the script fails", func(f *files) {
			f.savework = func(string) (string, int) { return "", 2 }
		}, func(*agentd.Config) {}, "reading the workspace failed"),
		Entry("no koinon-api address", func(f *files) {
			f.savework = func(string) (string, int) { return "R acme/widgets " + baseSHA + "\nF A 100644 1 1 0 0 x\nx\n\nE\n", 0 }
		}, func(c *agentd.Config) { c.SavedWorkURL = "" }, "not configured"),
		Entry("the agent has no access to the repository", func(f *files) {
			f.savework = func(string) (string, int) {
				return "R other/repo " + baseSHA + "\nF A 100644 1 1 0 0 x\nx\n\nE\n", 0
			}
		}, func(*agentd.Config) {}, "no GitHub access to the repository"),
		Entry("only unsupported changes", func(f *files) {
			f.savework = func(string) (string, int) {
				return "S acme/widgets: link: a symbolic link or submodule is not saved\n", 0
			}
		}, func(*agentd.Config) {}, "symbolic link"),
	)

	It("says nothing was lost when the workspace is clean", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		e.stuckAfterTurn(dProc)
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(func() string {
			var w string
			_ = db.QueryRow(ctx, `SELECT work FROM sandbox_incidents WHERE org_id = $1`, e.org).Scan(&w)
			return w
		}).WithTimeout(racescale.D(5 * time.Second)).Should(Equal("nothing"))
		Expect(kindsOf(taskEvents(ctx, e, t))).NotTo(ContainElement("sandbox.work_lost"))
	})

	It("stops recreating after three in 24 hours: the agent is unhealthy, owners are told, new runs fail", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		t := e.task(ctx, "hello")
		first := e.enqueue(ctx, t)
		other := e.task(ctx, "waiting")
		waiting := e.enqueue(ctx, other) // queued behind the first run
		// Three earlier recreations.
		for range 3 {
			_, err := db.Exec(ctx, `INSERT INTO sandbox_incidents (org_id, agent_id, run_id, task_id, processes, outcome, work)
				VALUES ($1, $2, $3, $4, '[]', 'recreate', 'nothing')`, e.org, e.agent.ID, first.ID, t.ID)
			Expect(err).NotTo(HaveOccurred())
		}
		e.stuckAfterTurn(dProc)
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, first)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		Eventually(func() bool {
			a, _ := q.GetAgent(ctx, store.GetAgentParams{OrgID: e.org, ID: e.agent.ID})
			return a.UnhealthyAt.Valid
		}).WithTimeout(racescale.D(5 * time.Second)).Should(BeTrue())
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.UnhealthyAt.Valid).To(BeFalse(), "the loop guard: the sandbox is not recreated a fourth time")
		var kind, payload string
		Expect(db.QueryRow(ctx, `SELECT kind, payload::text FROM notifications WHERE org_id = $1 AND user_id = $2`, e.org, e.user).Scan(&kind, &payload)).To(Succeed())
		Expect(kind).To(Equal("agent.unhealthy"))
		Expect(payload).To(ContainSubstring(e.agent.ID.String()))
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElement("agent.unhealthy"))
		Expect(kindsOf(taskEvents(ctx, e, other))).To(ContainElement("agent.unhealthy"), "a task whose run waits is told why")
		Expect(getRun(ctx, waiting).State).To(Equal(store.RunStateQueued))
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.mark_unhealthy'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))

		// A run queued meanwhile is not claimed: it waits until an admin resumes the agent.
		e.message(ctx, t, "again")
		second := e.enqueue(ctx, t)
		Consistently(runState(ctx, second)).WithTimeout(1500 * time.Millisecond).Should(Equal(store.RunStateQueued))
		e.files.mu.Lock()
		e.files.sweep, e.files.procinfo = nil, nil // the cause is fixed
		e.files.mu.Unlock()
		_, err = db.Exec(ctx, `UPDATE agents SET unhealthy_at = NULL, unhealthy_reason = NULL WHERE id = $1`, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(runState(ctx, second)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(runState(ctx, waiting)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("recreates before the next run when the process is found by the sweep that precedes the harness", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		sweeps := 0
		e.files.sweep = func() (string, int) {
			sweeps++
			if sweeps == 1 { // the first run's pre-run sweep finds a survivor
				return "survivors: " + stuckPID + "\nstill running: 41\n", 1
			}
			return "ended: none\n", 0
		}
		e.files.procinfo = func() string { return dProc }
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a", fastStuck(receiverURL()))

		// The reconciler's part: the sandbox flagged unhealthy is replaced.
		old := e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId()
		Eventually(func() bool {
			sb, _ := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
			return sb.UnhealthyAt.Valid
		}).WithTimeout(racescale.D(10 * time.Second)).Should(BeTrue())
		Expect(runState(ctx, r)()).NotTo(Equal(store.RunStateSucceeded), "the run waits for the new sandbox")
		e.recreateSandbox(ctx)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionGeneration.String).NotTo(Equal(old), "the session lives in the new home")
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.UnhealthyAt.Valid).To(BeFalse(), "the mark ends with the sandbox it was made for")
	})

	It("lets a parked run resume in the new sandbox through the reset path", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, bound)
		t := e.task(ctx, "hello")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a", fastStuck(receiverURL()))
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		// The home the session lives in is replaced (as the reconciler does for a stuck sandbox).
		e.recreateSandbox(ctx)
		e.message(ctx, t, "after")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElement("run.session_reset"))
	})
})

var _ = Describe("procinfo.sh", func() {
	It("prints state, command and wait channel of the processes it is given, and nothing for the others", func() {
		proc := GinkgoT().TempDir()
		write := func(pid, stat, comm, wchan string) {
			dir := filepath.Join(proc, pid)
			Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "stat"), []byte(stat+"\n"), 0o600)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o600)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "wchan"), []byte(wchan), 0o600)).To(Succeed()) // no newline, as the kernel writes it
		}
		// Fields after "(comm) ": state, ppid, ..., the 20th is the start time.
		stat := func(pid, comm, state, start string) string {
			return fmt.Sprintf("%s (%s) %s 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 %s 1000 100", pid, comm, state, start)
		}
		write("41", stat("41", "my (odd) proc", "D", "98765"), "my (odd) proc", "nfs_wait_bit_killable")
		write("42", stat("42", "sleeper", "S", "555"), "sleeper", "0")
		write("43", stat("43", "ghost", "Z", "777"), "ghost", "")
		write("44", stat("44", "reused", "R", "999999"), "reused", "")

		cmd := exec.Command("sh", "-c", agentd.ProcinfoScript, "procinfo", "41:98765", "42:555", "43:777", "44:111", "45:1")
		cmd.Env = append(os.Environ(), "KOINON_PROC="+proc)
		out, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Split(strings.TrimSpace(string(out)), "\n")).To(Equal([]string{
			"41|D|my (odd) proc|nfs_wait_bit_killable",
			"42|S|sleeper|0",
		}), "a zombie, a reused PID and a missing process print nothing")
		Expect(agentd.ProcinfoOf(string(out))).To(HaveLen(2))
	})

	It("reads the sweep's survivors line", func() {
		Expect(agentd.SurvivorsOf("main: 7\nsurvivors: 41:98765 55:12\n")).To(Equal([]string{"41:98765", "55:12"}))
		Expect(agentd.SurvivorsOf("ended: none\n")).To(BeEmpty())
		Expect(agentd.SurvivorsOf("survivors: x:y 41\n")).To(BeEmpty())
	})
})

var _ = Describe("savework.sh", func() {
	It("lists the uncommitted work against the newest commit GitHub has, leaving the repository alone", func() {
		testenv.Require(testenv.Binary("git"))
		work := GinkgoT().TempDir()
		repo := filepath.Join(work, "widgets")
		git := func(args ...string) string {
			c := exec.Command("git", append([]string{"-C", repo}, args...)...)
			c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
			out, err := c.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
			return strings.TrimSpace(string(out))
		}
		put := func(name, body string) {
			Expect(os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600)).To(Succeed())
		}
		Expect(os.MkdirAll(repo, 0o755)).To(Succeed())
		git("init", "-q", "-b", "main", ".")
		put("a.txt", "one\n")
		put("gone.txt", "bye\n")
		put("b c.txt", "two\n")
		git("add", "-A")
		git("commit", "-qm", "init")
		git("remote", "add", "origin", "https://github.com/acme/widgets.git")
		git("update-ref", "refs/remotes/origin/main", "HEAD")
		base := git("rev-parse", "HEAD")
		put("local.txt", "committed but not pushed\n")
		git("add", "local.txt")
		git("commit", "-qm", "local")
		put("a.txt", "one\nchanged\n")
		Expect(os.Remove(filepath.Join(repo, "gone.txt"))).To(Succeed())
		put("d/new.txt", "new\n")
		put(".gitignore", "*.log\n")
		put("x.log", "ignored\n")
		put("bin.dat", "\x00\x01\x02")
		Expect(os.Symlink("a.txt", filepath.Join(repo, "link"))).To(Succeed())
		status := git("status", "--short")

		out, err := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "1000000").Output()
		Expect(err).NotTo(HaveOccurred())
		works, skipped, err := agentd.ParseSaveOutput(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(works).To(HaveLen(1))
		m := works[0].Manifest
		Expect(m.Repo).To(Equal("acme/widgets"))
		Expect(m.Base).To(Equal(base), "the commit GitHub has, not the local one")
		byPath := map[string]savedwork.File{}
		for _, f := range m.Files {
			byPath[f.Path] = f
		}
		Expect(byPath).To(HaveKey("local.txt"))
		Expect(byPath["a.txt"]).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{"Status": Equal("M"), "Added": Equal(1), "Deleted": Equal(0)}))
		Expect(byPath["gone.txt"].Status).To(Equal("D"))
		Expect(byPath["d/new.txt"].Status).To(Equal("A"))
		Expect(byPath["bin.dat"].Binary).To(BeTrue())
		Expect(byPath).NotTo(HaveKey("x.log"), "ignored files stay out")
		Expect(string(works[0].Content["a.txt"])).To(Equal("one\nchanged\n"))
		Expect(string(works[0].Content["bin.dat"])).To(Equal("\x00\x01\x02"))
		Expect(skipped).To(ContainElement(ContainSubstring("link: a symbolic link")))
		Expect(m.Validate()).To(Succeed())
		Expect(git("status", "--short")).To(Equal(status), "the repository's index and tree are untouched")

		over, err := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "10").CombinedOutput()
		var ee *exec.ExitError
		Expect(errors.As(err, &ee)).To(BeTrue())
		Expect(ee.ExitCode()).To(Equal(3), string(over))
	})

	Describe("with a repository whose configuration runs code", func() {
		// A canary appends to a file when git runs it and otherwise behaves like cat.
		var work, repo, canary string
		BeforeEach(func() {
			testenv.Require(testenv.Binary("git"))
			work = GinkgoT().TempDir()
			repo = filepath.Join(work, "widgets")
			canary = filepath.Join(GinkgoT().TempDir(), "ran")
			Expect(os.MkdirAll(repo, 0o755)).To(Succeed())
		})
		script := func(dir, name string) string {
			p := filepath.Join(dir, name)
			Expect(os.WriteFile(p, []byte("#!/bin/sh\necho \"$0 $*\" >> "+canary+"\ncat\n"), 0o700)).To(Succeed())
			return p
		}
		git := func(args ...string) {
			c := exec.Command("git", append([]string{"-C", repo}, args...)...)
			c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
			out, err := c.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
		}
		put := func(name, body string) {
			Expect(os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600)).To(Succeed())
		}
		setup := func() {
			git("init", "-q", "-b", "main", ".")
			put(".gitattributes", "*.txt filter=evil diff=evil\n")
			put("a.txt", "one\n")
			git("add", "-A")
			git("commit", "-qm", "init")
			git("remote", "add", "origin", "https://github.com/acme/widgets.git")
			git("update-ref", "refs/remotes/origin/main", "HEAD")
			put("a.txt", "one\nraw edit\n")
			put("b.txt", "new\n")
		}
		run := func(env ...string) ([]byte, error) {
			c := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "1000000")
			c.Env = append(os.Environ(), env...)
			return c.Output()
		}
		ran := func() string {
			b, _ := os.ReadFile(canary)
			return string(b)
		}

		It("runs none of the repository's fsmonitor, hooks, filters or diff drivers", func() {
			setup()
			tools := GinkgoT().TempDir()
			hooks := filepath.Join(tools, "hooks")
			Expect(os.MkdirAll(hooks, 0o755)).To(Succeed())
			for _, h := range []string{"post-index-change", "pre-commit", "reference-transaction"} {
				script(hooks, h)
			}
			c := script(tools, "canary")
			for k, v := range map[string]string{
				"core.fsmonitor": c, "core.hooksPath": hooks,
				"filter.evil.clean": c, "filter.evil.smudge": c, "filter.evil.process": c, "filter.evil.required": "true",
				"diff.evil.textconv": c, "diff.evil.command": c,
				"core.pager": c, "core.sshCommand": c, "core.askPass": c,
			} {
				git("config", k, v)
			}
			git("config", "diff.external", c)

			out, err := run()
			Expect(err).NotTo(HaveOccurred())
			Expect(ran()).To(BeEmpty(), "git ran something the repository's configuration names")
			works, skipped, err := agentd.ParseSaveOutput(out)
			Expect(err).NotTo(HaveOccurred())
			Expect(skipped).To(BeEmpty())
			Expect(works).To(HaveLen(1))
			Expect(string(works[0].Content["a.txt"])).To(Equal("one\nraw edit\n"), "files are read as they are on disk")
			Expect(string(works[0].Content["b.txt"])).To(Equal("new\n"))
			Expect(works[0].Manifest.Files).To(HaveLen(2))
		})

		It("skips a repository with a filter driver named *, which must not expand to file names", func() {
			setup()
			c := script(GinkgoT().TempDir(), "canary")
			put(".gitattributes", "*.txt filter=*\n")
			git("config", "filter.*.clean", c)
			// Run where a * matches names that are valid driver names, as / or the home would.
			cwd := GinkgoT().TempDir()
			for _, f := range []string{"bin", "etc", "work"} {
				Expect(os.WriteFile(filepath.Join(cwd, f), nil, 0o600)).To(Succeed())
			}
			cmd := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "1000000")
			cmd.Dir = cwd
			out, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(ran()).To(BeEmpty())
			works, skipped, err := agentd.ParseSaveOutput(out)
			Expect(err).NotTo(HaveOccurred())
			Expect(works).To(BeEmpty())
			Expect(skipped).To(ConsistOf(ContainSubstring("cannot neutralise")))
		})

		It("fetches nothing for a partial clone, whatever its promisor remote runs", func() {
			setup()
			// Not the cat-like canary: an upload-pack that waits on its input would hang the fetch.
			c := filepath.Join(GinkgoT().TempDir(), "canary")
			Expect(os.WriteFile(c, []byte("#!/bin/sh\necho \"$0 $*\" >> "+canary+"\nexit 1\n"), 0o700)).To(Succeed())
			blob, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD:a.txt").Output()
			Expect(err).NotTo(HaveOccurred())
			id := strings.TrimSpace(string(blob))
			Expect(os.Remove(filepath.Join(repo, ".git", "objects", id[:2], id[2:]))).To(Succeed())
			for k, v := range map[string]string{
				"core.repositoryformatversion": "1", "extensions.partialClone": "evil",
				"remote.evil.url": GinkgoT().TempDir(), "remote.evil.promisor": "true", "remote.evil.uploadpack": c,
			} {
				git("config", k, v)
			}
			_, err = run()
			Expect(err).NotTo(HaveOccurred())
			Expect(ran()).To(BeEmpty(), "git fetched from the promisor remote")
		})

		It("skips a repository whose configuration includes another file, running nothing", func() {
			setup()
			tools := GinkgoT().TempDir()
			c := script(tools, "canary")
			inc := filepath.Join(tools, "inc.cfg")
			Expect(os.WriteFile(inc, []byte("[core]\n\tfsmonitor = "+c+"\n"), 0o600)).To(Succeed())
			for _, args := range [][]string{{"include.path", inc}, {"includeIf.gitdir:" + repo + "/.path", inc}} {
				git("config", args[0], args[1])
				out, err := run()
				Expect(err).NotTo(HaveOccurred())
				Expect(ran()).To(BeEmpty())
				works, skipped, err := agentd.ParseSaveOutput(out)
				Expect(err).NotTo(HaveOccurred())
				Expect(works).To(BeEmpty())
				Expect(skipped).To(ConsistOf(ContainSubstring("includes other files")))
				git("config", "--unset", args[0])
			}
		})

		It("ignores the system, global and home configuration, including the include file the home git config is rendered into", func() {
			setup()
			tools := GinkgoT().TempDir()
			c := script(tools, "canary")
			// What the agent can write: the file /etc/gitconfig includes, ~/.gitconfig and XDG config.
			home := filepath.Join(tools, "home")
			Expect(os.MkdirAll(filepath.Join(home, ".config", "git"), 0o755)).To(Succeed())
			koinon := filepath.Join(tools, "koinon-git-config")
			evil := "[core]\n\tfsmonitor = " + c + "\n\thooksPath = " + tools + "\n[filter \"evil\"]\n\tclean = " + c + "\n"
			for _, f := range []string{koinon, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config")} {
				Expect(os.WriteFile(f, []byte(evil), 0o600)).To(Succeed())
			}
			system := filepath.Join(tools, "gitconfig")
			Expect(os.WriteFile(system, []byte("[include]\n\tpath = "+koinon+"\n"), 0o600)).To(Succeed())

			// Unset by the script's own export, so these would apply to any git that read them.
			out, err := run("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_SYSTEM="+system,
				"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ran()).To(BeEmpty())
			works, skipped, err := agentd.ParseSaveOutput(out)
			Expect(err).NotTo(HaveOccurred())
			Expect(skipped).To(BeEmpty())
			Expect(string(works[0].Content["a.txt"])).To(Equal("one\nraw edit\n"))
		})
	})

	It("reads nothing from a repository whose origin is not GitHub, and says so", func() {
		testenv.Require(testenv.Binary("git"))
		work := GinkgoT().TempDir()
		for _, args := range [][]string{{"init", "-q", "."}, {"remote", "add", "origin", "https://gitlab.example.test/a/b.git"}} {
			c := exec.Command("git", append([]string{"-C", work}, args...)...)
			Expect(c.Run()).To(Succeed())
		}
		out, err := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "1000").Output()
		Expect(err).NotTo(HaveOccurred())
		works, skipped, err := agentd.ParseSaveOutput(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(works).To(BeEmpty())
		Expect(skipped).To(ContainElement(ContainSubstring("not a repository on github.com")))
	})

	It("reads the repositories of the connection's host, and only those", func() {
		testenv.Require(testenv.Binary("git"))
		work := GinkgoT().TempDir()
		for _, origin := range []string{"https://ghes.example.test:8443/acme/widgets.git", "git@ghes.example.test:acme/gadgets", "https://github.com/acme/other.git"} {
			dir := filepath.Join(work, filepath.Base(strings.TrimSuffix(origin, ".git")))
			Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
			git := func(args ...string) {
				c := exec.Command("git", append([]string{"-C", dir}, args...)...)
				c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
					"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
				out, err := c.CombinedOutput()
				Expect(err).NotTo(HaveOccurred(), string(out))
			}
			git("init", "-q", "-b", "main", ".")
			Expect(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600)).To(Succeed())
			git("add", "-A")
			git("commit", "-qm", "init")
			git("remote", "add", "origin", origin)
			git("update-ref", "refs/remotes/origin/main", "HEAD")
			Expect(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600)).To(Succeed())
		}
		out, err := exec.Command("sh", "-c", agentd.SaveworkScript, "savework", work, "1000000", "ghes.example.test:8443").Output()
		Expect(err).NotTo(HaveOccurred())
		works, skipped, err := agentd.ParseSaveOutput(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(works).To(HaveLen(1))
		Expect(works[0].Manifest.Repo).To(Equal("acme/widgets"))
		Expect(skipped).To(HaveLen(2))
		Expect(skipped).To(ContainElement(ContainSubstring("not a repository on ghes.example.test:8443")))
	})

	It("rejects output that ends inside a file", func() {
		_, _, err := agentd.ParseSaveOutput([]byte("R a/b " + baseSHA + "\nF A 100644 10 1 0 0 x\nshort\n"))
		Expect(err).To(HaveOccurred())
	})
})
