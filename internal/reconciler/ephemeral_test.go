// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
)

// Ephemeral agents (plan item EP1, ADR 0065 decision 13c): a sandbox per task, deleted with its
// volume when the task ends, and never reused by the next task.
var _ = Describe("Reconciler ephemeral agents", func() {
	var (
		osg  *fakegateway.Gateway
		org  pgtype.UUID
		user pgtype.UUID
		r    *reconciler.Reconciler
		ws   string
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	sandboxOf := func(agent store.Agent, run store.Run) string {
		return execution.TaskSandboxName(org.Bytes, agent.ID.Bytes, run.TaskID.Bytes)
	}
	exists := func(name string) bool { return osg.Sandbox(ws, name) != nil }
	row := func(ctx context.Context, agent store.Agent) store.AgentSandbox {
		GinkgoHelper()
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		return sb
	}
	finish := func(ctx context.Context, run store.Run) {
		GinkgoHelper()
		_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateSucceeded})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		r, err = reconciler.New(pool, openshell.New(conn), &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, reconciler.Config{
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: fake.NewClientset(), Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			URLPolicy:      testPolicy,
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			RuntimeClassName: "kata",
			Logger:           slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("runs two consecutive tasks in two sandboxes, and the first is gone after its task ends", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
		persistent := execution.AgentSandboxName(org.Bytes, agent.ID.Bytes)

		pass(ctx)
		Expect(exists(persistent)).To(BeFalse(), "an ephemeral agent has no sandbox of its own")
		Expect(osg.Calls("CreateSandbox")).To(BeZero(), "and none before a task needs one")

		queueRun(ctx, org, user, agent.ID)
		pass(ctx) // the row points at the task's sandbox, which is rendered in the same pass
		first := claimRun(ctx, org)
		firstName := sandboxOf(agent, first)
		pass(ctx)
		Expect(exists(firstName)).To(BeTrue())
		Expect(row(ctx, agent).SandboxName).To(Equal(firstName))
		firstID := osg.Sandbox(ws, firstName).GetMetadata().GetId()

		finish(ctx, first)
		pass(ctx)
		Expect(exists(firstName)).To(BeTrue(), "kept between runs: the task is still open")
		_, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: org, ID: first.TaskID, State: store.TaskStateCompleted})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(exists(firstName)).To(BeFalse(), "deleted when the task ends, workspace volume with it")

		queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		second := claimRun(ctx, org)
		secondName := sandboxOf(agent, second)
		pass(ctx)
		Expect(secondName).NotTo(Equal(firstName))
		Expect(exists(secondName)).To(BeTrue())
		Expect(osg.Sandbox(ws, secondName).GetMetadata().GetId()).NotTo(Equal(firstID), "a new sandbox, a new home")
		Expect(exists(firstName)).To(BeFalse())
		Expect(row(ctx, agent).SandboxName).To(Equal(secondName))
		Expect(osg.Calls("CreateSandbox")).To(Equal(2))
	})

	It("waits for the previous sandbox to be deleted before it renders the next", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
		a := queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		claimed := claimRun(ctx, org)
		Expect(claimed.ID).To(Equal(a.ID))
		finish(ctx, claimed)
		_, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: org, ID: a.TaskID, State: store.TaskStateCompleted})
		Expect(err).NotTo(HaveOccurred())
		b := queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		Expect(exists(sandboxOf(agent, a))).To(BeFalse())
		pass(ctx)
		Expect(exists(sandboxOf(agent, b))).To(BeTrue())
		Expect(row(ctx, agent).SandboxName).To(Equal(sandboxOf(agent, b)))
	})

	It("keeps one sandbox across two runs of one open task, and deletes it when the task ends", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
		a := queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		first := claimRun(ctx, org)
		pass(ctx)
		name := sandboxOf(agent, first)
		Expect(exists(name)).To(BeTrue())
		id := osg.Sandbox(ws, name).GetMetadata().GetId()
		finish(ctx, first)
		pass(ctx)
		Expect(exists(name)).To(BeTrue(), "the task is open between runs")

		// A follow-up message: a second run on the same task.
		_, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: org, AgentID: agent.ID, TaskID: a.TaskID, MaxWallClockSeconds: 1800,
		})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		second := claimRun(ctx, org)
		Expect(second.TaskID).To(Equal(a.TaskID))
		pass(ctx)
		Expect(exists(name)).To(BeTrue())
		Expect(osg.Sandbox(ws, name).GetMetadata().GetId()).To(Equal(id), "the same sandbox and home")
		finish(ctx, second)
		pass(ctx)
		Expect(osg.Calls("CreateSandbox")).To(Equal(1))

		_, err = q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: org, ID: a.TaskID, State: store.TaskStateCompleted})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		Expect(exists(name)).To(BeFalse())
	})

	It("keeps a persistent agent's one sandbox across tasks", func(ctx SpecContext) {
		agent := newAgent(ctx, org, user)
		persistent := execution.AgentSandboxName(org.Bytes, agent.ID.Bytes)
		pass(ctx)
		for range 2 {
			queueRun(ctx, org, user, agent.ID)
			run := claimRun(ctx, org)
			pass(ctx)
			finish(ctx, run)
			pass(ctx)
			Expect(exists(persistent)).To(BeTrue())
		}
		Expect(osg.Calls("CreateSandbox")).To(Equal(1))
	})
})
