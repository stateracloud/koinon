// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
)

// The crash and liveness path (issue #1481).
var _ = Describe("the crash and liveness path", func() {
	var (
		d  *substrate.Driver
		cl *fakeate.Cluster
	)
	BeforeEach(func() {
		dd, c := newDriver()
		d, cl = dd, c
	})

	Describe("the first boot", func() {
		It("takes one synchronous suspend, so a revert never gives an empty home", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			Expect(cl.Calls("SuspendActor")).To(Equal(1))
			sb, err := d.GetSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).NotTo(Equal(execution.PhaseSuspended), "the actor runs again after the seal")
			// A second resume of a sealed actor takes no new suspend.
			_, err = d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.StartSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Calls("SuspendActor")).To(Equal(2))
		})
	})

	Describe("CheckFront", func() {
		It("is nil while the front answers", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			Expect(d.CheckFront(ctx, ws, "sb")).To(Succeed())
		})
		It("reports ErrFrontDown for a running actor whose pid 1 exited", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.KillFront(ws, "sb")
			err := d.CheckFront(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFrontDown)).To(BeTrue(), "got %v", err)
		})
		It("does not resume a suspended actor", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			_, err := d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			err = d.CheckFront(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Expect(cl.RouterResumes(ws, "sb")).To(BeZero())
		})
	})

	Describe("a dead front", func() {
		It("is faulted and reverted to the last snapshot", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.KillFront(ws, "sb")
			Expect(d.FaultSandbox(ctx, ws, "sb")).To(Succeed())
			sb, err := d.GetSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseError))
			Expect(substrate.ClassifyCrash(sb)).To(Equal(substrate.CrashOther))
			res, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.SnapshotAt.IsZero()).To(BeFalse(), "the driver took the snapshot, so it knows its time")
			Expect(d.CheckFront(ctx, ws, "sb")).To(Succeed())
		})
		It("refuses a revert of an actor that never suspended", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.DropSnapshots(ws, "sb") // as for an actor whose first boot was not sealed
			cl.Crash(ws, "sb", "wakeup probe failed")
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrNoSnapshot)).To(BeTrue(), "got %v", err)
			Expect(cl.Calls("RevertActor")).To(BeZero())
		})
		It("refuses a revert of an actor that is not crashed", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
		})
		It("retries a revert that Substrate refuses with ResourceExhausted", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.KillFront(ws, "sb")
			Expect(d.FaultSandbox(ctx, ws, "sb")).To(Succeed())
			cl.FailNextReverts(ws, "sb", 2)
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Calls("RevertActor")).To(Equal(3))
		})
	})

	Describe("ClassifyCrash", func() {
		It("decides by phase and message, not by a gRPC code", func() {
			crashed := func(msg string) execution.Sandbox {
				return execution.Sandbox{Phase: execution.PhaseError, Reason: "ActorCrashed", Message: msg}
			}
			Expect(substrate.ClassifyCrash(crashed("wakeup probe: GET /readyz: connection refused"))).To(Equal(substrate.CrashProbe))
			Expect(substrate.ClassifyCrash(crashed("something else"))).To(Equal(substrate.CrashOther))
			Expect(substrate.ClassifyCrash(execution.Sandbox{Phase: execution.PhaseReady, Message: "wakeup probe"})).To(BeEmpty())
		})
	})
})
