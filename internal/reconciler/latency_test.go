// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/wake"
)

// Latency defaults (plan item LT1, ADRs 0049, 0065, 0066): the warm window per park reason, the
// per-org tunables, the pre-wake signal, and sandbox jobs that do not wait for a gateway reload.
var _ = Describe("Reconciler latency defaults", func() {
	var (
		osg     *fakegateway.Gateway
		backend *openshell.Backend
		mg      *fakeModelGateway
		now     atomic.Pointer[time.Time]
		cfg     reconciler.Config
		org     pgtype.UUID
		user    pgtype.UUID
		r       *reconciler.Reconciler
	)
	advance := func(d time.Duration) {
		t := time.Now().Add(d)
		now.Store(&t)
	}
	phase := func(agent pgtype.UUID) openshellv1.SandboxPhase {
		names := execution.AgentObjectNames(org.Bytes, agent.Bytes)
		return osg.Sandbox(execution.WorkspaceName(org.Bytes), names.Sandbox).GetStatus().GetPhase()
	}
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		backend = openshell.New(conn)
		mg = &fakeModelGateway{}
		now.Store(nil)
		cfg = reconciler.Config{
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Flavors: map[store.AgentFlavor]reconciler.Flavor{
				store.AgentFlavorOpencode: {Image: image, Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			},
			RuntimeClassName: "kata",
			Logger:           slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now: func() time.Time {
				if t := now.Load(); t != nil {
					return *t
				}
				return time.Now()
			},
		}
		org, user = newOrg(ctx)
		addProvider(ctx, org, user, "upstream", "", "fake")
		kube := fake.NewClientset()
		var err2 error
		r, err2 = reconciler.New(pool, backend, mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg)
		Expect(err2).NotTo(HaveOccurred())
	})

	// parked brings the agent to a Ready sandbox with a run parked for reason.
	parked := func(ctx context.Context, reason store.RunParkReason) store.Agent {
		GinkgoHelper()
		agent := newAgent(ctx, org, user)
		pass(ctx)
		queueRun(ctx, org, user, agent.ID)
		run := claimRun(ctx, org)
		_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: reason})
		Expect(err).NotTo(HaveOccurred())
		return agent
	}

	DescribeTable("keeps a sandbox warm for the park reason's window",
		func(ctx SpecContext, reason store.RunParkReason, stoppedAt11, stoppedAt16 bool) {
			agent := parked(ctx, reason)
			advance(9 * time.Minute)
			pass(ctx)
			Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "within the idle timeout")

			advance(11 * time.Minute)
			pass(ctx)
			Expect(phase(agent.ID) == openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED).To(Equal(stoppedAt11), "11 minutes after the park")

			advance(16 * time.Minute)
			pass(ctx)
			Expect(phase(agent.ID) == openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED).To(Equal(stoppedAt16), "16 minutes after the park")
		},
		Entry("a2a: 15 minutes", store.RunParkReasonA2a, false, true),
		Entry("input_required: 15 minutes", store.RunParkReasonInputRequired, false, true),
		Entry("approval: the 10 minute idle timeout", store.RunParkReasonApproval, true, true),
		Entry("budget: the 10 minute idle timeout", store.RunParkReasonBudget, true, true),
	)

	It("takes the idle timeout and the park window from the org's settings", func(ctx SpecContext) {
		idle, warm := 2*time.Minute, 30*time.Minute
		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{IdleTimeout: &idle, ParkWarm: &warm})).To(Succeed())

		a2a := parked(ctx, store.RunParkReasonA2a)
		advance(20 * time.Minute)
		pass(ctx)
		Expect(phase(a2a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "warm for the org's 30 minutes")
		advance(31 * time.Minute)
		pass(ctx)
		Expect(phase(a2a.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))

		now.Store(nil)
		approval := parked(ctx, store.RunParkReasonApproval)
		advance(3 * time.Minute)
		pass(ctx)
		Expect(phase(approval.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED), "idle after the org's 2 minutes")
	})

	// askParked brings the agent to a Ready sandbox with a run parked on an ask_user question
	// (TK3): the question is created, opened and the run parked as agentd does at the turn's end.
	askParked := func(ctx context.Context) (store.Agent, store.Run, store.Question) {
		GinkgoHelper()
		agent := newAgent(ctx, org, user)
		pass(ctx)
		queued := queueRun(ctx, org, user, agent.ID)
		run := claimRun(ctx, org)
		var row store.Question
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			qq := store.New(tx)
			var err error
			row, _, err = questions.Create(ctx, qq, questions.Ask{
				Org: org, Task: queued.TaskID, Run: run.ID, Agent: agent.ID, To: user,
				Question: "Which region?", Choices: []string{"eu", "us"}, Default: "eu",
			})
			if err != nil {
				return err
			}
			if _, err := questions.OpenForRun(ctx, qq, org, run.ID, queued.TaskID); err != nil {
				return err
			}
			_, err = qq.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonInputRequired})
			return err
		})).To(Succeed())
		return agent, run, row
	}
	answer := func(ctx context.Context, row store.Question) {
		GinkgoHelper()
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := questions.Respond(ctx, store.New(tx), org, row.ID, user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us"})
			return err
		})).To(Succeed())
	}

	It("resumes an ask_user question answered within the warm window without a sandbox start", func(ctx SpecContext) {
		agent, run, row := askParked(ctx)
		creates, stops := osg.Calls("CreateSandbox"), osg.Calls("StopSandbox")
		advance(14 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "still running 14 minutes after the park")

		answer(ctx, row)
		now.Store(nil)
		pass(ctx)
		got, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ReadyAt.Valid).To(BeTrue(), "the run is claimable")
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
		Expect(osg.Calls("CreateSandbox")).To(Equal(creates), "no sandbox was created for the answer")
		Expect(osg.Calls("StopSandbox")).To(Equal(stops), "and none was stopped")
	})

	It("starts the stopped sandbox for an ask_user answer that comes after the warm window, without recreating it", func(ctx SpecContext) {
		agent, _, row := askParked(ctx)
		creates := osg.Calls("CreateSandbox")
		advance(16 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED), "stopped after the 15 minute window")

		answer(ctx, row)
		now.Store(nil)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "the answer starts it for the resumed run")
		Expect(osg.Calls("CreateSandbox")).To(Equal(creates), "the home volume stays: agentd resumes with session/load")
	})

	It("counts a pre-wake signal as activity, and starts a stopped sandbox for it", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		advance(9 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

		By("a signal near the idle deadline restarts the idle clock")
		woke, err := wake.Agent(ctx, q, org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(woke).To(BeTrue())
		// The signal is stamped with the database clock; place it 9 minutes in, on the test's clock.
		_, err = pool.Exec(ctx, `UPDATE agent_sandboxes SET woken_at = $2 WHERE agent_id = $1`, agent.ID, time.Now().Add(9*time.Minute))
		Expect(err).NotTo(HaveOccurred())
		advance(12 * time.Minute) // 3 minutes after the signal, 12 after the start
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "kept up by the signal")

		By("a stopped sandbox starts for a signal, before any run exists")
		advance(30 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		now.Store(nil)
		_, err = wake.CronDue(ctx, q, org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))

		By("and stops again when no run follows")
		advance(11 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		advance(12 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED), "an old signal does not start it again")
	})

	It("starts a stopped sandbox while the gateway is still reloading", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		pass(ctx)
		advance(11 * time.Minute)
		pass(ctx)
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		now.Store(nil)
		queueRun(ctx, org, user, agent.ID)

		hold, entered := make(chan struct{}), make(chan struct{})
		mg.mu.Lock()
		mg.hold, mg.entered = hold, entered
		mg.mu.Unlock()
		done := make(chan error, 1)
		go func() { done <- r.ReconcileAll(ctx) }()
		Eventually(entered).Should(BeClosed(), "the pass is waiting for the gateway")

		Eventually(func() openshellv1.SandboxPhase { return phase(agent.ID) }).WithTimeout(10*time.Second).
			Should(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY), "the start did not wait for the gateway reload")
		Consistently(done, 200*time.Millisecond).ShouldNot(Receive(), "the gateway is still reloading")

		close(hold)
		Eventually(done).WithTimeout(10 * time.Second).Should(Receive(BeNil()))
	})
	// Early jobs run while the gateway Apply is still in flight. They may do that only because the
	// sandbox's policy names the gateway's MCP route whatever tools any agent has (the grants
	// are applied by the Apply, not by the sandbox): so a policy rendered by an early job cannot
	// name a route the gateway has not applied. This pins that ordering assumption.
	It("renders the same sandbox policy in an early job as before, whatever tools the Apply is changing", func(ctx SpecContext) {
		cfg.ToolServers = map[string]string{"github": toolsURL}
		tr, err := reconciler.New(pool, backend, mg, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		agent := newAgent(ctx, org, user)
		Expect(tr.ReconcileAll(ctx)).To(Succeed())
		routes := func() []string {
			var paths []string
			for _, rule := range osg.Profile(orgWorkspace(ctx, org), "koinon-agentgateway-opencode").GetEndpoints()[0].GetRules() {
				paths = append(paths, rule.GetAllow().GetMethod()+" "+rule.GetAllow().GetPath())
			}
			return paths
		}
		before := routes()
		Expect(before).To(ContainElement("POST /mcp"), "the route is named although no agent has a tool grant")

		By("a tool grant arrives while an early job restarts the stopped sandbox")
		advance(11 * time.Minute)
		Expect(tr.ReconcileAll(ctx)).To(Succeed())
		Expect(phase(agent.ID)).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		now.Store(nil)
		grant := newAgentWithConfig(ctx, org, user, `{"instructions":"help","grants":[`+
			`{"connector":"github","scopes":["repo.read","pull_request.create"],"preset":"standard"}]}`)
		queueRun(ctx, org, user, agent.ID)
		hold, entered := make(chan struct{}), make(chan struct{})
		mg.mu.Lock()
		mg.hold, mg.entered = hold, entered
		mg.mu.Unlock()
		done := make(chan error, 1)
		go func() { done <- tr.ReconcileAll(ctx) }()
		Eventually(entered).Should(BeClosed())
		Eventually(func() openshellv1.SandboxPhase { return phase(agent.ID) }).WithTimeout(10 * time.Second).
			Should(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
		Expect(routes()).To(Equal(before), "the early job's policy does not depend on the Apply")
		Expect(grant.ID.Valid).To(BeTrue())
		close(hold)
		Eventually(done).WithTimeout(10 * time.Second).Should(Receive(BeNil()))
	})
})
