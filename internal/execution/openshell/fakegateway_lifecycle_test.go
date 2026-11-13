// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// wsScope selects the suite's workspace.
func wsScope() *datamodelv1.WorkspaceSelector {
	return &datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_Workspace{Workspace: ws}}
}

// These specs call the fake gateway directly, not through the backend, whose settle step
// would hide a fake that is stricter than the real gateway (issue 439). The expected
// results are those of OpenShell v0.1.7 compute/mod.rs: QuiesceOperation::admit (stop) and
// start_sandbox_with_intent (start).
var _ = Describe("fakegateway stop and start", func() {
	var (
		gw     *fakegateway.Gateway
		client openshellv1.OpenShellClient
	)

	BeforeEach(func(ctx SpecContext) {
		gw = fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		client = openshellv1.NewOpenShellClient(conn)
		b := openshell.New(conn)
		_, err = b.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.CreateSandbox(ctx, ws, execution.SandboxSpec{
			Name: "sb", Image: "img", Command: []string{"sleep", "infinity"}, Policy: harnessPolicy(),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	stop := func(ctx context.Context) (*openshellv1.SandboxResponse, error) {
		return client.StopSandbox(ctx, &openshellv1.StopSandboxRequest{WorkspaceScope: wsScope(), Name: "sb"})
	}
	start := func(ctx context.Context) (*openshellv1.SandboxResponse, error) {
		return client.StartSandbox(ctx, &openshellv1.StartSandboxRequest{WorkspaceScope: wsScope(), Name: "sb"})
	}

	DescribeTable("stop",
		func(ctx SpecContext, from openshellv1.SandboxPhase) {
			gw.SetPhase(ws, "sb", from)
			resp, err := stop(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetSandbox().GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED))
		},
		Entry("a ready sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_READY),
		Entry("an already stopped sandbox is settled", openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED),
		Entry("a completed sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_COMPLETED),
		Entry("a stopping sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPING),
		Entry("a sandbox in error", openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR),
		Entry("a suspending sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_SUSPENDING),
		Entry("a suspended sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_SUSPENDED),
	)

	DescribeTable("stop refused",
		func(ctx SpecContext, from openshellv1.SandboxPhase) {
			gw.SetPhase(ws, "sb", from)
			_, err := stop(ctx)
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
		},
		Entry("a provisioning sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_PROVISIONING),
		Entry("a starting sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_STARTING),
		Entry("a deleting sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_DELETING),
	)

	DescribeTable("start",
		func(ctx SpecContext, from openshellv1.SandboxPhase) {
			gw.SetPhase(ws, "sb", from)
			resp, err := start(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetSandbox().GetStatus().GetPhase()).To(Equal(openshellv1.SandboxPhase_SANDBOX_PHASE_READY))
		},
		Entry("a stopped sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED),
		Entry("an already ready sandbox is settled", openshellv1.SandboxPhase_SANDBOX_PHASE_READY),
		Entry("a completed sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_COMPLETED),
		Entry("a starting sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_STARTING),
		Entry("a suspended sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_SUSPENDED),
	)

	DescribeTable("start refused",
		func(ctx SpecContext, from openshellv1.SandboxPhase) {
			gw.SetPhase(ws, "sb", from)
			_, err := start(ctx)
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
		},
		Entry("a sandbox in error", openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR),
		Entry("a provisioning sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_PROVISIONING),
		Entry("a stopping sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPING),
		Entry("a suspending sandbox", openshellv1.SandboxPhase_SANDBOX_PHASE_SUSPENDING),
	)
})

// The gateway finds a sandbox's draft chunks by its name and clears only pending ones; after the
// sandbox is deleted both calls are NotFound (OpenShell 5d25211b2), which the backend reads as
// "none", so chunks must be cleared before the delete (old issue 640).
var _ = Describe("backend draft policy chunks", func() {
	var (
		gw *fakegateway.Gateway
		b  *openshell.Backend
	)

	BeforeEach(func(ctx SpecContext) {
		gw = fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		b = openshell.New(conn)
		_, err = b.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.CreateSandbox(ctx, ws, execution.SandboxSpec{
			Name: "sb", Image: "img", Command: []string{"sleep", "infinity"}, Policy: harnessPolicy(),
		})
		Expect(err).NotTo(HaveOccurred())
		gw.SeedDraftChunk(ws, "sb", "c1", "pending")
		gw.SeedDraftChunk(ws, "sb", "c2", "pending")
		gw.SeedDraftChunk(ws, "sb", "c3", "approved")
	})

	It("counts and clears the pending chunks only", func(ctx SpecContext) {
		Expect(b.PendingDraftChunks(ctx, ws, "sb")).To(Equal(2))
		Expect(b.ClearDraftChunks(ctx, ws, "sb")).To(BeEquivalentTo(2))
		Expect(b.PendingDraftChunks(ctx, ws, "sb")).To(BeZero())
		Expect(b.ClearDraftChunks(ctx, ws, "sb")).To(BeZero())
	})

	It("reads a deleted sandbox as having none", func(ctx SpecContext) {
		_, err := b.DeleteSandbox(ctx, ws, "sb")
		Expect(err).NotTo(HaveOccurred())
		Expect(b.PendingDraftChunks(ctx, ws, "sb")).To(BeZero())
		Expect(b.ClearDraftChunks(ctx, ws, "sb")).To(BeZero())
	})

	It("lists the sandbox names of a workspace", func(ctx SpecContext) {
		Expect(b.ListSandboxNames(ctx, ws)).To(ConsistOf("sb"))
	})
})
