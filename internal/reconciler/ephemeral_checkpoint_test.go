// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
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

// Ephemeral agents with home checkpoints on (HC1 with EP1, ADR 0032 and 0065 decision 13c): a task
// sandbox has no home that outlives it, so it is never backed up, gated or restored.
var _ = Describe("Reconciler ephemeral agents with checkpoints on", func() {
	var (
		osg  *fakegateway.Gateway
		org  pgtype.UUID
		user pgtype.UUID
		cfg  reconciler.Config
		r    *reconciler.Reconciler
		ws   string
	)
	pass := func(ctx context.Context) {
		GinkgoHelper()
		Expect(r.ReconcileAll(ctx)).To(Succeed())
	}
	var backend *openshell.Backend

	BeforeEach(func(ctx SpecContext) {
		osg = fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		backend = openshell.New(conn)
		org, user = newOrg(ctx)
		ws = execution.WorkspaceName(org.Bytes)
		addProvider(ctx, org, user, "upstream", "", "fake")
		cfg = reconciler.Config{
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
			Logger:           slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
			Checkpoints:      true,
		}
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
	})

	It("files no checkpoint request, does not gate a recreation, and starts each task without a restore", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help","ephemeral":true}`)
		a := queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		first := claimRun(ctx, org)
		pass(ctx)
		firstName := execution.TaskSandboxName(org.Bytes, agent.ID.Bytes, first.TaskID.Bytes)
		Expect(osg.Sandbox(ws, firstName)).NotTo(BeNil())

		By("the hourly request skips it")
		rows, err := q.RequestHourlyHomeCheckpoints(ctx, store.RequestHourlyHomeCheckpointsParams{OrgID: org, IntervalSeconds: 3600})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())

		By("a spec change recreates the sandbox without asking for a backup")
		_, err = q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: first.ID, Epoch: first.Epoch, Outcome: store.RunStateSucceeded})
		Expect(err).NotTo(HaveOccurred())
		cfg.Harnesses[harness.OpenCode] = reconciler.Harness{Image: image + "-2", Binaries: reconciler.DefaultBinaries[harness.OpenCode]}
		r, err = reconciler.New(pool, backend, &fakeModelGateway{}, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
		_, err = q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))

		By("a restore left pending on the row does not reach the next task's sandbox")
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		cp, err := q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{OrgID: org, AgentID: agent.ID, Reason: store.CheckpointReasonHourly, SandboxID: sb.SandboxID.String, Generation: sb.HomeGeneration.String})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: org, AgentID: agent.ID, CheckpointID: cp.ID})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: org, ID: a.TaskID, State: store.TaskStateCompleted})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		queueRun(ctx, org, user, agent.ID)
		pass(ctx)
		pass(ctx)
		got, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.RestoreCheckpointID.Valid).To(BeFalse())
		Expect(got.HomeGeneration.String).To(Equal(got.SandboxID.String), "the task sandbox starts its own lineage")
	})
	It("backs up a persistent home before the agent becoming ephemeral deletes it", func(ctx SpecContext) {
		agent := newAgentWithConfig(ctx, org, user, `{"instructions":"help"}`)
		persistent := execution.AgentSandboxName(org.Bytes, agent.ID.Bytes)
		pass(ctx)
		Expect(osg.Sandbox(ws, persistent)).NotTo(BeNil())
		dirtyHome(ctx, org, user, agent.ID)

		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: org, AgentID: agent.ID, Config: []byte(`{"instructions":"help","ephemeral":true}`), CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(BeZero(), "kept until its home is backed up")
		cp, err := q.GetOpenHomeCheckpoint(ctx, store.GetOpenHomeCheckpointParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(cp.Reason).To(Equal(store.CheckpointReasonRecreate))

		cp, err = q.ClaimHomeCheckpoint(ctx, store.ClaimHomeCheckpointParams{OrgID: org, LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent.ID})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CompleteHomeCheckpoint(ctx, store.CompleteHomeCheckpointParams{
			OrgID: org, ID: cp.ID, SnapshotID: pgtype.Text{String: "koinon_test_snapshot", Valid: true},
			SizeBytes: pgtype.Int8{Int64: 10, Valid: true}, Generation: sb.HomeGeneration.String,
		})
		Expect(err).NotTo(HaveOccurred())
		pass(ctx)
		pass(ctx)
		Expect(osg.Calls("DeleteSandbox")).To(Equal(1))
	})
})
