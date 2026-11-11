// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/pricing"
)

// chunks returns the text of the run's agent message chunks, in order.
func chunks(evs []store.RunEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Kind != "acp.agent_message_chunk" {
			continue
		}
		var u struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		Expect(json.Unmarshal(e.Payload, &u)).To(Succeed())
		out = append(out, u.Content.Text)
	}
	return out
}

// expectOrdered checks that run events are numbered 1..n without gaps and that epochs never
// go back.
func expectOrdered(evs []store.RunEvent) {
	var epoch int64
	for i, e := range evs {
		Expect(e.Seq).To(BeEquivalentTo(i+1), "event %d (%s)", i, e.Kind)
		Expect(e.Epoch).To(BeNumerically(">=", epoch))
		epoch = e.Epoch
	}
}

func payload(e store.RunEvent) map[string]any {
	var m map[string]any
	Expect(json.Unmarshal(e.Payload, &m)).To(Succeed())
	return m
}

func eventOf(evs []store.RunEvent, kind string) []store.RunEvent {
	var out []store.RunEvent
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

var _ = Describe("Pool", func() {
	It("drives a task's first run: new session, prompt, updates, permissions, end_turn", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markPermit+" please")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		// The launch command and its endpoints, run in the session directory.
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(Equal([][]string{{
			"--model-base-url", gatewayURL + "/v1", "--model", model, "--mcp-url", gatewayURL + "/mcp", "--model-tries", "3", "--model-retry-backoff-ms", "1000", "--model-max-retry-after-ms", "60000", "--model-format", "openai", "--thinking", "none", "--no-web-tools",
		}}))
		// The sweep, mkdir, the instruction file and its check, the git configuration and the CA
		// file's removal (no custom host), the harness, the instruction file's check
		// before the prompt, then the sweep after the harness has ended.
		Eventually(e.gw.Execs).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(9))
		execs := e.gw.Execs()
		sweep := []string{"/bin/sh", "-c", agentd.SweepScript, "sweep"}
		read := HaveExactElements("/bin/sh", "-c", ContainSubstring("exec cat"), "sh", instructionsFile)
		Expect(execs[0].GetCommand()).To(Equal(sweep))
		Expect(execs[1].GetCommand()).To(Equal([]string{"/bin/mkdir", "-p", "/sandbox/work"}))
		Expect(execs[2].GetCommand()).To(HaveExactElements("/bin/sh", "-c", ContainSubstring("mktemp"), "sh", "/sandbox/.koinon", instructionsFile))
		Expect(execs[3].GetCommand()).To(read)
		Expect(execs[4].GetCommand()).To(HaveExactElements("/bin/sh", "-c", ContainSubstring("koinon-gitconfig"), "sh", "/sandbox/.koinon/git", "/sandbox/.koinon/git/config"))
		Expect(execs[5].GetCommand()).To(HaveExactElements("/bin/sh", "-c", ContainSubstring("koinon-gitca"), "sh", "/sandbox/.koinon/git", "/sandbox/.koinon/git/ca.pem"))
		Expect(execs[6].GetCommand()[0]).To(Equal("/usr/local/bin/koinon-opencode"))
		Expect(execs[6].GetWorkdir()).To(Equal("/sandbox/work"))
		Expect(execs[6].GetTty()).To(BeFalse())
		Expect(execs[6].GetEnvironment()).To(BeEmpty(), "agentd sends nothing secret, and no environment, into the sandbox")
		Expect(execs[7].GetCommand()).To(read)
		Expect(execs[8].GetCommand()).To(Equal(sweep))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
		Expect(e.gw.Sandbox(e.workspace, e.sandbox).GetSpec().GetEnvironment()).To(BeEmpty())

		// The session is stored on the task.
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionID.String).To(Equal("ses_1"))
		Expect(got.HarnessSessionCwd.String).To(Equal("/sandbox/work"))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Task: " + t.Title + "\n\n" + markPermit + " please"}))
		Expect(loads).To(BeEmpty())

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt",
			"acp.agent_message_chunk",
			"acp.tool_call", "acp.permission",
			"acp.tool_call", "acp.permission",
			"acp.agent_message_chunk",
			"run.summary", "run.succeeded",
		}))
		Expect(payload(evs[0])).To(HaveKeyWithValue("loaded", false))
		perms := eventOf(evs, "acp.permission")
		Expect(payload(perms[0])["decision"]).To(HaveKeyWithValue("verdict", "allow"))
		Expect(payload(perms[0])["decision"]).To(HaveKeyWithValue("tier", "sandbox-write"))
		Expect(payload(perms[1])["decision"]).To(HaveKeyWithValue("verdict", "reject"), "fetch is unmapped")
		Expect(chunks(evs)).To(Equal([]string{"asking: ", "edit=allow,fetch=reject"}))

		final := getRun(ctx, r)
		Expect(final.PromptThroughSeq.Int64).To(BeEquivalentTo(1))
		Expect(final.FinishedAt.Valid).To(BeTrue())

		// The agent's reply lands on the task: its last message, without the narration before its tool calls.
		tevs := taskEvents(ctx, e, t)
		Expect(tevs).To(HaveLen(3))
		Expect(tevs[2].Kind).To(Equal("status.changed"), "the run's success completes the task (old issue 324)")
		Expect(string(tevs[2].Payload)).To(ContainSubstring(`"completed"`))
		Expect(tevs[1].Kind).To(Equal("message"))
		Expect(tevs[1].ActorKind).To(Equal(store.TaskActorKindAgent))
		Expect(string(tevs[1].Payload)).To(Equal(`{"text": "edit=allow,fetch=reject"}`))
	})

	It("sends a trigger's instruction, a system message carrying its source, as the first prompt", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t, err := q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: e.org, Title: uniq("Scheduled"), AgentID: e.agent.ID, CreatedByKind: store.TaskCreatorKindTrigger,
			CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{7}, Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: e.org, TaskID: t.ID, Kind: "message", ActorKind: store.TaskActorKindSystem,
			Payload: []byte(`{"text":"summarise yesterday's issues","source":"schedule"}`),
		})
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Task: " + t.Title + "\n\nsummarise yesterday's issues"}))
	})

	It("loads the task's session on its next run and sends only the new messages", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		e.message(ctx, t, "second")
		e.message(ctx, t, "third")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Task: " + t.Title + "\n\nfirst", "second\n\nthird"}))
		Expect(loads).To(Equal([]string{"ses_1"}))

		// The replayed history is not recorded again.
		evs := events(ctx, r2)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{"run.session", "run.prompt", "acp.agent_message_chunk", "run.summary", "run.succeeded"}))
		Expect(payload(evs[0])).To(HaveKeyWithValue("loaded", true))

		// The session was stored in the sandbox's home generation, which still matches.
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionGeneration.String).To(Equal(e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId()))
		Expect(got.HarnessSessionGeneration.String).NotTo(BeEmpty())
		Expect(kindsOf(taskEvents(ctx, e, t))).NotTo(ContainElement("run.session_reset"))
	})

	It("starts a new session when the sandbox home was replaced since the session was stored", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		stop := e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		before, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())

		// The reconciler recreates the sandbox (a spec change); OpenShell deletes its home.
		e.recreateSandbox(ctx)
		home := e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId()
		Expect(home).NotTo(Equal(before.HarnessSessionGeneration.String))

		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		e.pool("agentd-b")
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(BeEmpty(), "no session/load into an empty home")
		Expect(prompts).To(Equal([]string{
			"Task: " + t.Title + "\n\nfirst",
			"Task: " + t.Title + "\n\nfirst\n\nsecond",
		}), "the new session gets the whole task")

		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionID.String).To(Equal("ses_2"))
		Expect(got.HarnessSessionGeneration.String).To(Equal(home))

		evs := events(ctx, r2)
		Expect(payload(evs[0])).To(HaveKeyWithValue("loaded", false))
		Expect(payload(evs[0])).To(HaveKeyWithValue("reset", true))

		var reset []store.TaskEvent
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.session_reset" {
				reset = append(reset, ev)
			}
		}
		Expect(reset).To(HaveLen(1))
		Expect(reset[0].ActorKind).To(Equal(store.TaskActorKindSystem))
		Expect(reset[0].RunID).To(Equal(r2.ID))
		var p map[string]string
		Expect(json.Unmarshal(reset[0].Payload, &p)).To(Succeed())
		Expect(p).To(HaveKeyWithValue("message", "The agent's workspace was reset, so this run starts a new session."))
		Expect(p).To(HaveKeyWithValue("previousSessionId", "ses_1"))
		Expect(p).To(HaveKeyWithValue("sessionId", "ses_2"))
	})

	It("starts a new session for a session stored without a home generation", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		stop := e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		stop()
		// A task from before home generations existed.
		_, err := db.Exec(ctx, `UPDATE tasks SET harness_session_generation = NULL WHERE org_id = $1 AND id = $2`, e.org, t.ID)
		Expect(err).NotTo(HaveOccurred())

		e.message(ctx, t, "second") // a run is queued by a message; one with none to read ends without a turn (#21)
		r2 := e.enqueue(ctx, t)
		e.pool("agentd-b")
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		_, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(BeEmpty())
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElement("run.session_reset"))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionGeneration.Valid).To(BeTrue())
	})

	It("asks the reconciler to start a stopped sandbox and waits for it before it execs", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.gw.SetPhase(e.workspace, e.sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))

		// The claimed run is the reconciler's demand; agentd also wakes it while it waits.
		conn, err := db.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, "LISTEN koinon_reconcile")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			wctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			n, err := conn.Conn().WaitForNotification(wctx)
			if err != nil {
				return ""
			}
			return n.Payload
		}).WithTimeout(racescale.D(5 * time.Second)).Should(Equal(e.org.String()))
		_, err = conn.Exec(ctx, "UNLISTEN koinon_reconcile")
		Expect(err).NotTo(HaveOccurred())
		Consistently(runState(ctx, r)).WithTimeout(300*time.Millisecond).Should(Equal(store.RunStateStarting),
			"agentd does not exec into a sandbox the reconciler has not reported ready")
		Expect(e.gw.Calls("ExecSandboxInteractive")).To(BeZero())

		// The reconciler starts it and records it ready.
		e.gw.SetPhase(e.workspace, e.sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_READY)
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.gw.Calls("StartSandbox")).To(BeZero(), "only the reconciler starts sandboxes")
		Expect(e.gw.Calls("GetSandbox")).To(BeZero(), "sandbox state comes from agent_sandboxes")
	})

	It("keeps a run in the queue, past the 5 minute wait, when the reconciler does not make its sandbox ready", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout = 5 * time.Minute; c.Now = clock.Now })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		Consistently(runState(ctx, r)).WithTimeout(300 * time.Millisecond).Should(Equal(store.RunStateStarting))

		clock.Advance(5*time.Minute + time.Second)
		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("run.requeued"))
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Or(Equal(store.RunStateQueued), Equal(store.RunStateStarting)),
			"the run waits in the queue; it does not fail")
		Expect(getRun(ctx, r).OutcomeReason.Valid).To(BeFalse())
		Expect(e.gw.Calls("StartSandbox")).To(BeZero())

		// Capacity returns: the run goes on.
		e.gw.SetPhase(e.workspace, e.sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_READY)
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("waits as long as the org's queue wait says", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		wait := 20 * time.Minute
		Expect(orgsettings.Set(ctx, q, e.org, orgsettings.Override{QueueWait: &wait})).To(Succeed())
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) { c.Now = clock.Now })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		clock.awaitWait()
		clock.Advance(6 * time.Minute)
		Consistently(func() []string { return kinds(events(ctx, r)) }).WithTimeout(500 * time.Millisecond).ShouldNot(ContainElement("run.requeued"))
		clock.Advance(15 * time.Minute)
		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("run.requeued"))
	})

	It("honours a cancel with session/cancel and records only the cut-off message after it", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markWait))
		e.pool("agentd-a")
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(1))

		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: e.org, ID: r.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { _, _, c, _ := e.harness.snapshot(); return c }).WithTimeout(racescale.D(5 * time.Second)).Should(Equal(1))

		// The message in progress ("working") is stored as a cut-off item, although the run is
		// canceled: a stopped run keeps what the agent said (old issue 344).
		Eventually(func() []string { return chunks(events(ctx, r)) }).WithTimeout(racescale.D(5 * time.Second)).Should(Equal([]string{"working"}))
		Consistently(func() []string { return kinds(events(ctx, r)) }, 500*time.Millisecond).
			Should(Equal([]string{"run.session", "run.prompt", "acp.agent_message_chunk"}))
		Expect(payload(events(ctx, r)[2])).To(HaveKeyWithValue("partial", true))
		final := getRun(ctx, r)
		Expect(final.State).To(Equal(store.RunStateCanceled))
		Expect(final.OutcomeReason.String).To(Equal("user canceled"))
	})

	It("cancels the run at the wall-clock cap", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 2,
		})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateCanceled))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("wall-clock cap"))
		_, _, cancels, _ := e.harness.snapshot()
		Expect(cancels).To(Equal(1))
		Expect(kinds(events(ctx, r))).To(HaveLen(4))
		Expect(kinds(events(ctx, r))[3]).To(Equal("run.canceled"))
		Expect(taskEvents(ctx, e, t)[1].Kind).To(Equal("run.canceled"))
	})

	It("resumes with session/load under a new epoch when the exec stream drops", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markCrash)
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))

		final := getRun(ctx, r)
		Expect(final.Takeovers).To(BeEquivalentTo(1))
		prompts, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(Equal([]string{"ses_1"}))
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(HavePrefix(resumeMarker), "the resumed turn is not the original prompt again")

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt", "acp.agent_message_chunk", "run.interrupted",
			"run.session", "run.prompt", "acp.agent_message_chunk", "run.summary", "run.succeeded",
		}))
		Expect(evs[4].Epoch).To(Equal(evs[0].Epoch + 1))
		Expect(payload(evs[3])).To(HaveKeyWithValue("exitCode", BeEquivalentTo(137)))
		Expect(payload(evs[5])).To(HaveKeyWithValue("resumed", true))
		Expect(chunks(evs)).To(Equal([]string{"before the crash ", "resumed"}))
		Expect(string(taskEvents(ctx, e, t)[1].Payload)).To(ContainSubstring("before the crash resumed"))
	})

	It("fails a run whose harness writes a line over 10 MiB, without a resume", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markLongLine))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(30 * time.Second)).Should(Equal(store.RunStateFailed))

		final := getRun(ctx, r)
		Expect(final.OutcomeReason.String).To(ContainSubstring("harness output line too long"))
		Expect(final.Takeovers).To(BeZero(), "the same line would come again: no resume")
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
	})

	It("lets another replica take over a killed worker's run, without duplicate events", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markHang)
		r := e.enqueue(ctx, t)
		killA := e.pool("agentd-a", func(c *agentd.Config) { c.KeepLeaseOnShutdown = true })
		Eventually(e.harness.hangs).WithTimeout(racescale.D(10 * time.Second)).Should(Receive())
		claimed := getRun(ctx, r)
		Expect(claimed.OwnerID.String).To(HavePrefix("agentd-a/"))

		// The worker stops; its lease runs out. The message it was writing ("one two ") is stored
		// as it stops, cut off, as one item (old issue 344).
		killA()
		e.pool("agentd-b")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))

		final := getRun(ctx, r)
		Expect(final.OwnerID.String).To(HavePrefix("agentd-b/"))
		Expect(final.Epoch).To(Equal(claimed.Epoch + 1))
		Expect(final.Takeovers).To(BeEquivalentTo(1))

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt", "acp.agent_message_chunk",
			"run.session", "run.prompt", "acp.agent_message_chunk", "run.summary", "run.succeeded",
		}))
		got := chunks(evs)
		Expect(got).To(HaveLen(2), "the replayed history is not recorded again")
		Expect(got[0]).To(HavePrefix("one "), "the worker may stop before the second chunk is handled")
		Expect(got[1]).To(Equal("resumed"))
		Expect(payload(evs[2])).To(HaveKeyWithValue("partial", true))
		for _, ev := range evs[:3] {
			Expect(ev.Epoch).To(Equal(claimed.Epoch))
		}
		for _, ev := range evs[3:] {
			Expect(ev.Epoch).To(Equal(claimed.Epoch + 1))
		}
		_, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("fails the run with a reason when the session cannot be loaded", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "hello")
		// Stored in the current home, so agentd loads it, but the harness does not know it.
		_, err := db.Exec(ctx, `UPDATE tasks SET harness_session_id = 'ses_gone', harness_session_cwd = '/sandbox/work',
			harness_session_generation = $3 WHERE org_id = $1 AND id = $2`,
			e.org, t.ID, e.gw.Sandbox(e.workspace, e.sandbox).GetMetadata().GetId())
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		final := getRun(ctx, r)
		Expect(final.OutcomeReason.String).To(HavePrefix("session/load: "))
		Expect(kinds(events(ctx, r))).To(Equal([]string{"run.failed"}))
		tevs := taskEvents(ctx, e, t)
		Expect(tevs[len(tevs)-2].Kind).To(Equal("run.failed"))
		Expect(tevs[len(tevs)-1].Kind).To(Equal("status.changed"), "a fatal run fails its task (old issue 324)")
	})

	DescribeTable("runs each flavor's entry script with the agent's endpoints",
		func(ctx SpecContext, flavor store.AgentFlavor, script string) {
			e := newEnv(ctx, flavor)
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			// The sweep, mkdir, the instruction file and its check, the git configuration and CA
			// file, the harness, the file's check before the prompt, then
			// the sweep after the harness.
			Eventually(e.gw.Execs).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(9))
			execs := e.gw.Execs()
			Expect(execs[6].GetCommand()).To(Equal([]string{
				script, "--model-base-url", gatewayURL + "/v1", "--model", model, "--mcp-url", gatewayURL + "/mcp", "--model-tries", "3", "--model-retry-backoff-ms", "1000", "--model-max-retry-after-ms", "60000", "--model-format", "openai", "--thinking", "none", "--no-web-tools",
			}))
			Expect(execs[6].GetWorkdir()).To(Equal("/sandbox/work"))
		},
		Entry("opencode", store.AgentFlavorOpencode, "/usr/local/bin/koinon-opencode"),
	)

	It("writes the run's instructions before the harness starts and records their hash", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "hello")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		content, ok := e.files.get(instructionsFile)
		Expect(ok).To(BeTrue())
		Expect(string(content)).To(HavePrefix("# Koinon\n"))
		Expect(string(content)).To(HaveSuffix("\n\n" + instructions + "\n"))
		sum := sha256.Sum256(content)
		session := eventOf(events(ctx, r1), "run.session")[0]
		Expect(payload(session)["instructions"]).To(Equal(map[string]any{
			"path": instructionsFile, "sha256": hex.EncodeToString(sum[:]),
		}))
		Expect(payload(session)).To(HaveKeyWithValue("model", model))

		// A new version applies at the next run, without touching the sandbox.
		v2, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: e.org, AgentID: e.agent.ID, CreatedBy: e.user,
			Config: []byte(`{"model":"` + model + `","instructions":"Version two."}`),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: e.org, ID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: e.org, ID: e.agent.ID, VersionID: v2.ID, ActivatedBy: e.user})
		Expect(err).NotTo(HaveOccurred())
		e.message(ctx, t, "again")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		content, _ = e.files.get(instructionsFile)
		Expect(string(content)).To(HaveSuffix("\n\nVersion two.\n"))
		Expect(e.gw.Calls("CreateSandbox")).To(Equal(1), "only newEnv created the sandbox")
	})

	It("renders the git configuration of the connection's host into the sandbox before the harness starts", func(ctx SpecContext) {
		const ca = "-----BEGIN CERTIFICATE-----\nMIIBkoinon-test-not-a-real-certificate\n-----END CERTIFICATE-----\n"
		e := newEnvWith(ctx, store.AgentFlavorOpencode, func(e *env) string {
			connID := uuid.New()
			cfg, err := json.Marshal(connections.GitHubConfig{
				AppID: 1, InstallationID: 2, Site: codehost.Site{BaseURL: "https://ghes.example.test", CABundle: ca},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
				OrgID: e.org, ID: pgtype.UUID{Bytes: connID, Valid: true}, Connector: "github", DisplayName: "ghes",
				Config: cfg, SecretEnvelope: []byte(`{}`), Source: store.ConfigSourceUi,
			})
			Expect(err).NotTo(HaveOccurred())
			return `{"instructions":"help","model":"` + model + `",` +
				`"grants":[{"connector":"github","scopes":["repo.read"],"preset":"standard"}],` +
				`"bindings":[{"connector":"github","connectionId":"` + connID.String() + `","targets":["acme/widgets"]}]}`
		})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		cfg, ok := e.files.get(codehost.SandboxGitConfig)
		Expect(ok).To(BeTrue())
		Expect(string(cfg)).To(ContainSubstring("[credential \"https://ghes.example.test\"]"))
		Expect(string(cfg)).To(ContainSubstring("koinon-git-credential --host ghes.example.test"))
		Expect(string(cfg)).To(ContainSubstring("sslCAInfo = " + codehost.SandboxGitCA))
		Expect(string(cfg)).NotTo(ContainSubstring("github.com"))
		bundle, ok := e.files.get(codehost.SandboxGitCA)
		Expect(ok).To(BeTrue())
		Expect(string(bundle)).To(Equal(ca))
	})

	It("ends what an earlier run left running before the harness starts, and records it", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.files.sweep = func() (string, int) { return "ended: 41 57\n", 0 }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		evs := events(ctx, r)
		swept := eventOf(evs, "run.sandbox_swept")
		Expect(swept).To(HaveLen(1))
		Expect(payload(swept[0])).To(HaveKeyWithValue("pids", ConsistOf("41", "57")))
		Expect(swept[0].Seq).To(BeNumerically("<", eventOf(evs, "run.session")[0].Seq), "before the harness starts")
	})

	It("sweeps after a run it finished even when its harness outlives the run's end", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// The harness ignores end-of-file: closing it takes two seconds, long enough for a
		// heartbeat to find the run finished.
		e.harness.set(func(h *harness) { h.lingers = true })
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(e.files.sweepCount).WithTimeout(racescale.D(10*time.Second)).Should(Equal(2), "before the harness and after it")
	})

	It("sweeps after a run before the agent's next run starts its harness", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// Closing each harness takes two seconds: long enough for the next run to start its
		// own if the run gave up the agent's slot before its sweep.
		e.harness.set(func(h *harness) { h.lingers = true })
		a := e.enqueue(ctx, e.task(ctx, "first"))
		b := e.enqueue(ctx, e.task(ctx, "second"))
		e.pool("agentd-a")
		Eventually(runState(ctx, a)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(runState(ctx, b)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(e.files.sweepCount).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(4))
		var order []string
		for _, x := range e.gw.Execs() {
			switch c := x.GetCommand(); {
			case c[0] == "/usr/local/bin/koinon-opencode":
				order = append(order, "harness")
			case len(c) == 4 && c[2] == agentd.SweepScript:
				order = append(order, "sweep")
			}
		}
		// Each run: a sweep, its harness, a sweep; the first run's last sweep comes before the
		// second run's harness, which it would otherwise end.
		Expect(order).To(Equal([]string{"sweep", "harness", "sweep", "sweep", "harness", "sweep"}))
	})

	It("fails the run, without starting the harness, when processes of an earlier run keep running", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.files.sweep = func() (string, int) { return "still running: 41\n", 1 }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(And(
			ContainSubstring("end the processes left in the sandbox"), ContainSubstring("still running: 41")))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	It("fails the run, without starting the harness, when the sweep cannot tell the sandbox's main process", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.files.sweep = func() (string, int) { return "sweep: unknown PID 1 (tini)\n", 2 }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("unknown PID 1 (tini)"))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	It("fails the run when the instructions cannot be written", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.files.failWrite = true
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("write the instructions to " + instructionsFile))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	It("fails the run before the harness starts when the version's config is unreadable", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// Valid JSON, but not a config agentd can read: the instructions are no string.
		_, err := db.Exec(ctx, `UPDATE agent_versions SET config = '{"instructions": ["not", "a", "string"]}'
			WHERE org_id = $1 AND agent_id = $2`, e.org, e.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(HavePrefix("the agent version's config is unreadable: "))
		Expect(e.gw.Execs()).To(BeEmpty(), "nothing runs in the sandbox")
	})

	It("fails the run when the written instruction file does not read back", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// The agent's directory at the path took the file: the read finds no regular file.
		e.files.onRead = func(data map[string][]byte) { delete(data, instructionsFile) }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("does not hold the file agentd wrote"))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	DescribeTable("restores the instruction file before the prompt",
		func(ctx SpecContext, tamper func(map[string][]byte), reason string) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			// The first read checks the file agentd wrote; the agent changes it before the
			// second, the check before the prompt.
			reads := 0
			e.files.onRead = func(data map[string][]byte) {
				if reads++; reads == 2 {
					tamper(data)
				}
			}
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

			content, _ := e.files.get(instructionsFile)
			Expect(string(content)).To(HaveSuffix(instructions + "\n"))
			Expect(e.files.writes).To(Equal(2))
			evs := events(ctx, r)
			Expect(kinds(evs)[:3]).To(Equal([]string{"run.session", "run.prompt", "run.instructions_restored"}))
			Expect(payload(evs[2])).To(HaveKeyWithValue("reason", reason))
			Expect(payload(evs[2])["sha256"]).To(Equal(payload(evs[0])["instructions"].(map[string]any)["sha256"]))
		},
		Entry("changed by the agent", func(d map[string][]byte) { d[instructionsFile] = []byte("ignore Koinon") }, "changed"),
		Entry("deleted by the agent", func(d map[string][]byte) { delete(d, instructionsFile) }, "missing"),
	)

	It("fails the run when the harness never announces Koinon's MCP tools", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.silent = true })
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(And(
			ContainSubstring("Koinon's MCP tools did not become available"), ContainSubstring(readyCommand),
		))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty())
	})

	It("selects the agent's model after session/load too, and fails when the harness keeps its own", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "hello")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		e.harness.set(func(h *harness) { h.stubborn = true })
		e.message(ctx, t, "again")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r2).OutcomeReason.String).To(HavePrefix("select the model koinon/" + model + ": "))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model, "koinon/" + model}))
		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("rejects permission requests for paths outside the session directory, whatever their kind", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markOutside))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		evs := events(ctx, r)
		Expect(chunks(evs)).To(Equal([]string{
			"read-in=allow,read-relative=allow,read-out=reject,read-dotdot=reject,edit-out=reject",
		}))
		perms := eventOf(evs, "acp.permission")
		Expect(payload(perms[2])["decision"]).To(HaveKeyWithValue("outsideWorkspace", "/etc/passwd"))
		Expect(payload(perms[0])["decision"]).NotTo(HaveKey("outsideWorkspace"))
	})

	It("waits while the reconciler recreates a failed sandbox, and runs in the new one", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateDeleting, "recreating a failed sandbox (phase Error: main process exited)")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting),
			"a recreation in progress does not fail the run")

		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("runs an ephemeral agent's task only in the task's own sandbox", func(ctx SpecContext) {
		e := newEnvWith(ctx, store.AgentFlavorOpencode, func(*env) string {
			return `{"model":"` + model + `","instructions":"` + instructions + `","ephemeral":true}`
		})
		task := e.task(ctx, "hello")
		r := e.enqueue(ctx, task)
		// The row still names a previous task's sandbox, which is ready: not this task's.
		prev := execution.TaskSandboxName(e.org.Bytes, e.agent.ID.Bytes, [16]byte{1})
		_, err := q.SetAgentSandboxName(ctx, store.SetAgentSandboxNameParams{OrgID: e.org, AgentID: e.agent.ID, SandboxName: prev})
		Expect(err).NotTo(HaveOccurred())
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting),
			"never runs in another task's sandbox")

		// The reconciler renders the task's sandbox and moves the row.
		name := execution.TaskSandboxName(e.org.Bytes, e.agent.ID.Bytes, task.ID.Bytes)
		_, err = e.backend.CreateSandbox(ctx, e.workspace, execution.SandboxSpec{Name: name, Image: "ghcr.io/stateracloud/koinon/flavors/opencode:test"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetAgentSandboxName(ctx, store.SetAgentSandboxNameParams{OrgID: e.org, AgentID: e.agent.ID, SandboxName: name})
		Expect(err).NotTo(HaveOccurred())
		e.sandbox = name
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("requeues a run after SandboxTimeout when the failed sandbox is never recreated", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateDeleting, "recreating a failed sandbox (phase Error)")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout = time.Second })
		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("run.requeued"))
		Expect(getRun(ctx, r).State).NotTo(Equal(store.RunStateFailed))
	})

	It("fails a run taken over while running whose sandbox stays unready, instead of requeueing it", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateStarting, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "dead-owner", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(r.ID))
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE runs SET lease_expires_at = now() - interval '1 minute' WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())

		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout = time.Second })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("sandbox not ready"))
		Expect(kinds(events(ctx, r))).NotTo(ContainElement("run.requeued"))
	})

	It("waits for a sandbox in error to be repaired", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateError, "image pull failed")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting),
			"a failed sandbox is repaired by the reconciler, so the run waits")

		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("requeues a run whose sandbox stays in error past SandboxTimeout", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateError, "image pull failed")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout = time.Second })
		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("run.requeued"))
		Expect(getRun(ctx, r).State).NotTo(Equal(store.RunStateFailed))
	})

	It("fails a run at once when its sandbox is in error and the agent is unhealthy", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateError, "image pull failed")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		// Mark it once the run waits on the sandbox, not before execute loads the agent (that
		// fails the run as unhealthy, a different path).
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateStarting))
		_, err := q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: e.org, ID: e.agent.ID, Reason: "the sandbox kept failing"})
		Expect(err).NotTo(HaveOccurred())
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal("sandbox error: image pull failed"))
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassSandboxUnavailable))
	})

	It("records a retried event once", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(r.ID))
		params := store.AppendRunEventOnceParams{
			OrgID: e.org, RunID: r.ID, Epoch: claimed.Epoch, Kind: "acp.agent_message_chunk", Payload: []byte(`{}`), SourceSeq: 1,
		}
		first, err := q.AppendRunEventOnce(ctx, params)
		Expect(err).NotTo(HaveOccurred())
		_, err = q.AppendRunEventOnce(ctx, params)
		Expect(err).To(HaveOccurred(), "the retry matches no rows")
		again, err := q.GetRunEventBySource(ctx, store.GetRunEventBySourceParams{OrgID: e.org, RunID: r.ID, Epoch: claimed.Epoch, SourceSeq: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ID).To(Equal(first.ID))
		params.SourceSeq = 2
		second, err := q.AppendRunEventOnce(ctx, params)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Seq).To(Equal(first.Seq+1), "no gap from the retry")

		// A released lease can be taken over at once, under a new epoch.
		_, err = q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		took, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: e.org, OwnerID: "test-2", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(took.Epoch).To(Equal(claimed.Epoch + 1))
		Expect(took.Takeovers).To(BeEquivalentTo(1))
		_, err = q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: e.org, RunID: r.ID, Epoch: claimed.Epoch, Kind: "acp.agent_message_chunk", Payload: []byte(`{}`), SourceSeq: 3,
		})
		Expect(err).To(HaveOccurred(), "the old epoch is fenced")
	})
})

var _ = Describe("New", func() {
	It("refuses a gateway URL with credentials", func() {
		_, err := agentd.New(db, nil, agentd.Config{OwnerID: "x", GatewayURL: "http://user:pw@gw:8080"})
		Expect(err).To(MatchError(ContainSubstring("user info")))
		_, err = agentd.New(db, nil, agentd.Config{OwnerID: "x", GatewayURL: "http://gw:8080?key=secret"})
		Expect(err).To(HaveOccurred())
		_, err = agentd.New(db, nil, agentd.Config{OwnerID: "x", GatewayURL: "gw:8080"})
		Expect(err).To(HaveOccurred())
	})

	It("wants a checkpoint Timeout shorter than the checkpoint Lease", func() {
		cfg := func(cp agentd.Checkpoints) agentd.Config {
			return agentd.Config{OwnerID: "x", GatewayURL: gatewayURL, Prices: prices.NewResolver(pricing.Embedded()), Checkpoints: &cp}
		}
		repos := fixedRepo{}
		_, err := agentd.New(db, nil, cfg(agentd.Checkpoints{Store: &checkpoint.Writer{}, Repositories: repos, Timeout: 20 * time.Minute, Lease: 20 * time.Minute}))
		Expect(err).To(MatchError(ContainSubstring("Checkpoints.Timeout must be shorter than Checkpoints.Lease")))
		_, err = agentd.New(db, nil, cfg(agentd.Checkpoints{Store: &checkpoint.Writer{}, Repositories: repos, Timeout: 19*time.Minute + 45*time.Second, Lease: 20 * time.Minute}))
		Expect(err).To(MatchError(ContainSubstring("Checkpoints.Timeout must be shorter than Checkpoints.Lease")), "room for the outcome record")
		_, err = agentd.New(db, nil, cfg(agentd.Checkpoints{Store: &checkpoint.Writer{}, Repositories: repos, Timeout: 30 * time.Minute}))
		Expect(err).To(MatchError(ContainSubstring("Checkpoints.Timeout")), "against the default Lease")
		_, err = agentd.New(db, nil, cfg(agentd.Checkpoints{Store: &checkpoint.Writer{}, Repositories: repos}))
		Expect(err).NotTo(HaveOccurred(), "the defaults")
	})
})
