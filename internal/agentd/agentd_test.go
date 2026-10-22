// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentd"
	"github.com/procoduck/koinon/internal/db/store"
	openshellv1 "github.com/procoduck/koinon/internal/gen/openshell/v1"
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
	It("drives a ticket's first run: new session, prompt, updates, permissions, end_turn", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, markPermit+" please")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")

		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		// The launch command and its endpoints, run in the session directory.
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(Equal([][]string{{
			"--model-base-url", gatewayURL + "/v1", "--model", model, "--mcp-url", gatewayURL + "/mcp",
		}}))
		// mkdir, the instruction file, the harness, then the instruction check before the prompt.
		execs := e.gw.Execs()
		Expect(execs).To(HaveLen(4))
		Expect(execs[0].GetCommand()).To(Equal([]string{"/bin/mkdir", "-p", "/sandbox/work"}))
		Expect(execs[1].GetCommand()).To(HaveExactElements("/bin/sh", "-c", ContainSubstring("mktemp"), "sh", "/sandbox/.koinon", instructionsFile))
		Expect(execs[2].GetCommand()[0]).To(Equal("/usr/local/bin/koinon-opencode"))
		Expect(execs[2].GetWorkdir()).To(Equal("/sandbox/work"))
		Expect(execs[2].GetTty()).To(BeFalse())
		Expect(execs[2].GetEnvironment()).To(BeEmpty(), "agentd sends nothing secret, and no environment, into the sandbox")
		Expect(execs[3].GetCommand()).To(HaveExactElements("/bin/sh", "-c", ContainSubstring("exec cat"), "sh", instructionsFile))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
		Expect(e.gw.Sandbox(e.workspace, e.sandbox).GetSpec().GetEnvironment()).To(BeEmpty())

		// The session is stored on the ticket.
		got, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.HarnessSessionID.String).To(Equal("ses_1"))
		Expect(got.HarnessSessionCwd.String).To(Equal("/sandbox/work"))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Ticket: " + t.Title + "\n\n" + markPermit + " please"}))
		Expect(loads).To(BeEmpty())

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt",
			"acp.agent_message_chunk",
			"acp.tool_call", "acp.permission",
			"acp.tool_call", "acp.permission",
			"acp.agent_message_chunk",
			"run.succeeded",
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

		// The agent's reply lands on the ticket.
		tevs := ticketEvents(ctx, e, t)
		Expect(tevs).To(HaveLen(2))
		Expect(tevs[1].Kind).To(Equal("message"))
		Expect(tevs[1].ActorKind).To(Equal(store.TicketActorKindAgent))
		Expect(string(tevs[1].Payload)).To(ContainSubstring("asking: edit=allow,fetch=reject"))
	})

	It("loads the ticket's session on its next run and sends only the new messages", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, "first")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		e.message(ctx, t, "second")
		e.message(ctx, t, "third")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Ticket: " + t.Title + "\n\nfirst", "second\n\nthird"}))
		Expect(loads).To(Equal([]string{"ses_1"}))

		// The replayed history is not recorded again.
		evs := events(ctx, r2)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{"run.session", "run.prompt", "acp.agent_message_chunk", "run.succeeded"}))
		Expect(payload(evs[0])).To(HaveKeyWithValue("loaded", true))
	})

	It("asks the reconciler to start a stopped sandbox and waits for it before it execs", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.gw.SetPhase(e.workspace, e.sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateStarting))

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
		}).WithTimeout(5 * time.Second).Should(Equal(e.org.String()))
		_, err = conn.Exec(ctx, "UNLISTEN koinon_reconcile")
		Expect(err).NotTo(HaveOccurred())
		Consistently(runState(ctx, r)).WithTimeout(300*time.Millisecond).Should(Equal(store.RunStateStarting),
			"agentd does not exec into a sandbox the reconciler has not reported ready")
		Expect(e.gw.Calls("ExecSandboxInteractive")).To(BeZero())

		// The reconciler starts it and records it ready.
		e.gw.SetPhase(e.workspace, e.sandbox, openshellv1.SandboxPhase_SANDBOX_PHASE_READY)
		e.recordSandbox(ctx, store.SandboxStateReady, "")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.gw.Calls("StartSandbox")).To(BeZero(), "only the reconciler starts sandboxes")
		Expect(e.gw.Calls("GetSandbox")).To(BeZero(), "sandbox state comes from agent_sandboxes")
	})

	It("fails a run whose sandbox the reconciler does not make ready in time", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.SandboxTimeout = time.Second })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(HavePrefix("sandbox not ready after 1s (stopped)"))
		Expect(e.gw.Calls("StartSandbox")).To(BeZero())
	})

	It("honours a cancel with session/cancel and records nothing after it", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.ticket(ctx, markWait))
		e.pool("agentd-a")
		Eventually(func() []string { return chunks(events(ctx, r)) }).WithTimeout(10 * time.Second).Should(Equal([]string{"working"}))

		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: e.org, ID: r.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { _, _, c, _ := e.harness.snapshot(); return c }).WithTimeout(5 * time.Second).Should(Equal(1))

		Consistently(func() []string { return kinds(events(ctx, r)) }, 500*time.Millisecond).
			Should(Equal([]string{"run.session", "run.prompt", "acp.agent_message_chunk"}))
		final := getRun(ctx, r)
		Expect(final.State).To(Equal(store.RunStateCanceled))
		Expect(final.OutcomeReason.String).To(Equal("user canceled"))
	})

	It("cancels the run at the wall-clock cap", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, markWait)
		r, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: e.org, AgentID: e.agent.ID, TicketID: t.ID, MaxWallClockSeconds: 2, MaxTokens: 1000,
		})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateCanceled))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("wall-clock cap"))
		_, _, cancels, _ := e.harness.snapshot()
		Expect(cancels).To(Equal(1))
		Expect(kinds(events(ctx, r))).To(HaveLen(4))
		Expect(kinds(events(ctx, r))[3]).To(Equal("run.canceled"))
		Expect(ticketEvents(ctx, e, t)[1].Kind).To(Equal("run.canceled"))
	})

	It("resumes with session/load under a new epoch when the exec stream drops", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, markCrash)
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))

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
			"run.session", "run.prompt", "acp.agent_message_chunk", "run.succeeded",
		}))
		Expect(evs[4].Epoch).To(Equal(evs[0].Epoch + 1))
		Expect(payload(evs[3])).To(HaveKeyWithValue("exitCode", BeEquivalentTo(137)))
		Expect(payload(evs[5])).To(HaveKeyWithValue("resumed", true))
		Expect(chunks(evs)).To(Equal([]string{"before the crash ", "resumed"}))
		Expect(string(ticketEvents(ctx, e, t)[1].Payload)).To(ContainSubstring("before the crash resumed"))
	})

	It("lets another replica take over a killed worker's run, without duplicate events", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, markHang)
		r := e.enqueue(ctx, t)
		killA := e.pool("agentd-a", func(c *agentd.Config) { c.KeepLeaseOnShutdown = true })
		Eventually(e.harness.hangs).WithTimeout(10 * time.Second).Should(Receive())
		Eventually(func() []string { return chunks(events(ctx, r)) }).Should(Equal([]string{"one ", "two "}))
		claimed := getRun(ctx, r)
		Expect(claimed.OwnerID.String).To(HavePrefix("agentd-a/"))

		// SIGKILL: the worker stops without another write; its lease runs out.
		killA()
		e.pool("agentd-b")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))

		final := getRun(ctx, r)
		Expect(final.OwnerID.String).To(HavePrefix("agentd-b/"))
		Expect(final.Epoch).To(Equal(claimed.Epoch + 1))
		Expect(final.Takeovers).To(BeEquivalentTo(1))

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt", "acp.agent_message_chunk", "acp.agent_message_chunk",
			"run.session", "run.prompt", "acp.agent_message_chunk", "run.succeeded",
		}))
		Expect(chunks(evs)).To(Equal([]string{"one ", "two ", "resumed"}), "the replayed history is not recorded again")
		for _, ev := range evs[:4] {
			Expect(ev.Epoch).To(Equal(claimed.Epoch))
		}
		for _, ev := range evs[4:] {
			Expect(ev.Epoch).To(Equal(claimed.Epoch + 1))
		}
		_, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("fails the run with a reason when the session cannot be loaded", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, "hello")
		_, err := db.Exec(ctx, `UPDATE tickets SET harness_session_id = 'ses_gone', harness_session_cwd = '/sandbox/work'
			WHERE org_id = $1 AND id = $2`, e.org, t.ID)
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		final := getRun(ctx, r)
		Expect(final.OutcomeReason.String).To(HavePrefix("session/load: "))
		Expect(kinds(events(ctx, r))).To(Equal([]string{"run.failed"}))
		tevs := ticketEvents(ctx, e, t)
		Expect(tevs[len(tevs)-1].Kind).To(Equal("run.failed"))
	})

	DescribeTable("runs each flavor's entry script with the agent's endpoints",
		func(ctx SpecContext, flavor store.AgentFlavor, script string) {
			e := newEnv(ctx, flavor)
			r := e.enqueue(ctx, e.ticket(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
			// mkdir, the instruction file, the harness, the instruction check.
			execs := e.gw.Execs()
			Expect(execs).To(HaveLen(4))
			Expect(execs[2].GetCommand()).To(Equal([]string{
				script, "--model-base-url", gatewayURL + "/v1", "--model", model, "--mcp-url", gatewayURL + "/mcp",
			}))
			Expect(execs[2].GetWorkdir()).To(Equal("/sandbox/work"))
		},
		Entry("opencode", store.AgentFlavorOpencode, "/usr/local/bin/koinon-opencode"),
		Entry("hermes", store.AgentFlavorHermes, "/usr/local/bin/koinon-hermes"),
		Entry("pi", store.AgentFlavorPi, "/usr/local/bin/koinon-pi"),
	)

	It("writes the run's instructions before the harness starts and records their hash", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, "hello")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
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
			Config: []byte(`{"model":{"name":"` + model + `"},"instructions":"Version two."}`),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: e.org, ID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: e.org, ID: e.agent.ID, VersionID: v2.ID, ActivatedBy: e.user})
		Expect(err).NotTo(HaveOccurred())
		e.message(ctx, t, "again")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		content, _ = e.files.get(instructionsFile)
		Expect(string(content)).To(HaveSuffix("\n\nVersion two.\n"))
		Expect(e.gw.Calls("CreateSandbox")).To(Equal(1), "only newEnv created the sandbox")
	})

	It("fails the run when the instructions cannot be written", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.files.failWrite = true
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("write the instructions to " + instructionsFile))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(BeEmpty(), "the harness never starts")
	})

	DescribeTable("restores the instruction file before the prompt",
		func(ctx SpecContext, tamper func(map[string][]byte), reason string) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			once := false
			e.files.onRead = func(data map[string][]byte) {
				if !once {
					once = true
					tamper(data)
				}
			}
			r := e.enqueue(ctx, e.ticket(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

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
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(And(
			ContainSubstring("Koinon's MCP tools did not become available"), ContainSubstring(readyCommand),
		))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty())
	})

	It("selects the agent's model after session/load too, and fails when the harness keeps its own", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.ticket(ctx, "hello")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		e.harness.set(func(h *harness) { h.stubborn = true })
		e.message(ctx, t, "again")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r2).OutcomeReason.String).To(HavePrefix("select the model koinon/" + model + ": "))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model, "koinon/" + model}))
		prompts, loads, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
		Expect(loads).To(Equal([]string{"ses_1"}))
	})

	It("rejects permission requests for paths outside the session directory, whatever their kind", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.ticket(ctx, markOutside))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		evs := events(ctx, r)
		Expect(chunks(evs)).To(Equal([]string{
			"read-in=allow,read-relative=allow,read-out=reject,read-dotdot=reject,edit-out=reject",
		}))
		perms := eventOf(evs, "acp.permission")
		Expect(payload(perms[2])["decision"]).To(HaveKeyWithValue("outsideWorkspace", "/etc/passwd"))
		Expect(payload(perms[0])["decision"]).NotTo(HaveKey("outsideWorkspace"))
	})

	It("fails a run whose sandbox the reconciler reports in error", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateError, "image pull failed")
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal("sandbox error: image pull failed"))
	})

	It("records a retried event once", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.ticket(ctx, "hello"))
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
})
