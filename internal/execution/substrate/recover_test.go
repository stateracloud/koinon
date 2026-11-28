// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
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
		It("reports the suspended actor when the resume after the seal fails", func(ctx SpecContext) {
			cl.FailNextResumesAfter(ws, "sb", 1, 1) // the first resume works, the one after the seal fails
			sb, err := d.CreateSandbox(ctx, ws, spec("sb"))
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended), "the real state, not the running one of the first resume")
			Expect(cl.State(ws, "sb")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(sb.NewSnapshot).To(BeTrue(), "the seal is a snapshot")
		})
		It("reports the seal as a new snapshot", func(ctx SpecContext) {
			sb, err := d.CreateSandbox(ctx, ws, spec("sb"))
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.NewSnapshot).To(BeTrue())
		})
	})

	Describe("a suspend", func() {
		It("reports a new snapshot", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			sb, err := d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.NewSnapshot).To(BeTrue())
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
		It("gives no verdict when the router answers the guest call itself", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.KillFront(ws, "sb")
			cl.RouterRefuse(ws, "sb", true)
			err := d.CheckFront(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFrontDown)).To(BeFalse(), "the router's own Unavailable says nothing about the front: %v", err)
			Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
		})
		It("gives no verdict during a router outage", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.StopRouter()
			err := d.CheckFront(ctx, ws, "sb")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, execution.ErrFrontDown)).To(BeFalse(), "no connection to the router says nothing about the front: %v", err)
		})
		It("takes only the router's reports of an unreachable guest as a dead front", func() {
			for msg, want := range map[string]bool{
				fakeate.FrontDownMessage: true, // Envoy's local reply, upstream CONNECT mode
				"unexpected HTTP status code received from server: 502 (Bad Gateway); transport: received unexpected content-type \"text/plain; charset=utf-8\"": true,  // atunnel's 502, forwarded
				"unexpected HTTP status code received from server: 503 (Service Unavailable)":                                                                    false, // an ext_proc refusal of the router
				`actor ws/sb unavailable`: false,
				`connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.1:443: connect: connection refused"`: false,
				"error reading from server: EOF": false,
			} {
				Expect(substrate.FrontUnreachable(status.Error(codes.Unavailable, msg))).To(Equal(want), msg)
			}
			Expect(substrate.FrontUnreachable(status.Error(codes.Internal, fakeate.FrontDownMessage))).To(BeFalse(), "only an Unavailable")
		})
		Describe("a front that takes the call and never answers", func() {
			BeforeEach(func() {
				dd, c := newDriver(substrate.Config{FrontCheckTimeout: 100 * time.Millisecond})
				d, cl = dd, c
			})
			It("is silence when the control API answered fast in the same call", func(ctx SpecContext) {
				ready(ctx, d, "sb")
				cl.KillFront(ws, "sb")
				cl.HangFront(ws, "sb")
				err := d.CheckFront(ctx, ws, "sb")
				Expect(errors.Is(err, execution.ErrFrontDown)).To(BeTrue(), "got %v", err)
			})
			It("is no verdict when the control API was slow in the same call", func(ctx SpecContext) {
				ready(ctx, d, "sb")
				cl.KillFront(ws, "sb")
				cl.HangFront(ws, "sb")
				held, release := cl.HoldNextGetActor(ws, "sb")
				go func() {
					defer GinkgoRecover()
					<-held
					time.Sleep(1200 * time.Millisecond) // slower than the fast bound of the control API
					release()
				}()
				err := d.CheckFront(ctx, ws, "sb")
				Expect(errors.Is(err, execution.ErrFrontDown)).To(BeFalse(), "a slow control plane says nothing about the front: %v", err)
				Expect(err).To(HaveOccurred())
			})
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

	Describe("RevertSandbox", func() {
		It("reverts a running actor with a dead front directly, with no suspend first", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "the seal of the first boot")
			cl.KillFront(ws, "sb")
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Calls("SuspendActor")).To(Equal(1), "no suspend: it would save the dead front over the last good snapshot")
			Expect(cl.Calls("RevertActor")).To(Equal(1))
			Expect(d.CheckFront(ctx, ws, "sb")).To(Succeed(), "the front of the snapshot answers")
		})
		It("keeps a dead front a dead front when someone suspends first (the fake saves it)", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.KillFront(ws, "sb")
			_, err := d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred(), "the fake suspends a dead front like any other")
			_, err = d.StartSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Calls("SuspendActor")).To(Equal(2))
			cl.Crash(ws, "sb", "x")
			_, err = d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			err = d.CheckFront(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFrontDown)).To(BeTrue(), "the newest snapshot holds the dead front: %v", err)
		})
		It("goes back to the snapshot: the files written after it are gone", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.PutFile(ws, "sb", "/home/a", []byte("after the seal"))
			cl.Crash(ws, "sb", "guest kernel panic")
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, ok := cl.File(ws, "sb", "/home/a")
			Expect(ok).To(BeFalse())
		})
		It("refuses a revert of an actor that never suspended", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.DropSnapshots(ws, "sb") // as for an actor whose first boot was not sealed
			cl.Crash(ws, "sb", "wakeup probe failed")
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrNoSnapshot)).To(BeTrue(), "got %v", err)
			Expect(cl.Calls("RevertActor")).To(BeZero())
		})
		It("refuses a revert of an actor that is suspended", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			_, err := d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
		})
		It("retries a revert that Substrate refuses with ResourceExhausted", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.Crash(ws, "sb", "guest kernel panic")
			cl.FailNextReverts(ws, "sb", 2)
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Calls("RevertActor")).To(Equal(3))
		})
		It("is ErrRevertResume when the wakeup probe fails after the revert", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.Crash(ws, "sb", "guest kernel panic")
			cl.FailProbe(ws, "sb", 1)
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrRevertResume)).To(BeTrue(), "got %v", err)
			Expect(cl.Calls("DeleteActor")).To(BeZero())
		})
		It("is ErrRevertResume when the egress policy write fails after the revert", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.Crash(ws, "sb", "guest kernel panic")
			cl.FailNextPolicyReads(ws, "sb", 10)
			_, err := d.RevertSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrRevertResume)).To(BeTrue(), "got %v", err)
			Expect(cl.Calls("RevertActor")).To(Equal(1), "the revert worked")
			Expect(cl.State(ws, "sb")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED), "the actor holds its snapshot")
			Expect(cl.Calls("DeleteActor")).To(BeZero())
		})
	})
})
