// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// The crash and liveness path of issue #1481. A Substrate actor does not signal when pid 1 (the guest
// front) exits: it stays RUNNING (lab-verified: pid 1 exit gives no crash signal). The driver gives the
// reconciler two calls: CheckFront finds that the front is silent, and RevertSandbox goes back to the
// last snapshot. RevertActor accepts a RUNNING actor (Substrate cmd/ateapi/internal/controlapi/
// workflow_revert.go, revertableStates), so a dead front is reverted directly. A SuspendActor first would
// save the broken state over the last good snapshot. RevertSandbox keeps the home (about 0.1 s), where a
// delete and a create gives an empty one.

var _ execution.Reverter = (*Driver)(nil)

// exhaustedRetries is how many times a call that Substrate refused with ResourceExhausted is sent again.
const exhaustedRetries = 4

// controlFastBound is how fast a control API read must be for a deadline on the guest call to count as
// silence of the front. A control plane that is slow says nothing about the front.
const controlFastBound = time.Second

// CheckFront implements execution.Reverter. It reads the actor first with the control API, which never
// resumes it, and calls the guest only when the actor runs. It asks for a process that does not exist:
// any answer of the front, NotFound included, proves that the front runs. Unavailable from the guest is
// silence. A deadline is silence only when the control API read of the same call was fast
// (controlFastBound): with a slow control plane the deadline is not a verdict, and the call returns
// ErrUnavailable. Any other error is not a verdict on the front and is returned as it is.
func (d *Driver) CheckFront(ctx context.Context, workspace, name string) error {
	const op = "CheckFront"
	k := key{workspace, name}
	// The gate proves that the actor runs and stops a suspend from starting under the call. A closed
	// gate or a sandbox that does not run is no verdict on the front.
	// enter is not used: it binds the actor with a guest call, and a dead front fails that.
	var gctx context.Context
	var end func()
	for ok := false; !ok; {
		g := d.gate(k)
		if gctx, end, ok = g.begin(ctx, shortCall); !ok && !g.isDead() {
			return fail(op, execution.ErrFailedPrecondition, "sandbox %s is being suspended or deleted", k)
		}
	}
	defer end()
	// The proof that the actor runs comes after the registration: a suspend that starts now drains
	// the call, so the router never resumes the actor for it.
	read := time.Now()
	actor, err := d.control.GetActor(gctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.GRPCError(op, err)
	}
	controlFast := time.Since(read) < controlFastBound
	if st := actor.GetStatus().GetState(); st != ateapiv1.ActorState_ACTOR_STATE_RUNNING {
		return fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not running", k, phaseOf(actor))
	}
	cctx, cancel := context.WithTimeout(gctx, d.cfg.FrontCheckTimeout)
	defer cancel()
	_, err = d.process.GetProcess(route(cctx, k), &ateenvv1alpha.GetProcessRequest{ProcessId: "koinon-liveness"})
	switch status.Code(err) {
	case codes.OK, codes.NotFound, codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated:
		return nil // the front answered
	case codes.Unavailable, codes.DeadlineExceeded:
		if ctx.Err() != nil {
			return execution.GRPCError(op, ctx.Err())
		}
		if derr := d.routerDown(op, err); derr != nil {
			return derr // the router is down: that says nothing about the front
		}
		if status.Code(err) == codes.DeadlineExceeded && !controlFast {
			return fail(op, execution.ErrUnavailable, "the guest call of sandbox %s ran into its deadline and the control API is slow too: no verdict on the front", k)
		}
		return fail(op, execution.ErrFrontDown, "the guest front of sandbox %s does not answer: %v", k, err)
	}
	return execution.GRPCError(op, err)
}

// RevertSandbox implements execution.Reverter. The actor must be CRASHED, or RUNNING with a dead front.
// It fails with execution.ErrNoSnapshot when the actor never suspended, because the revert would give an
// empty home. It sends the egress policy again (resume does), and it sends a call that Substrate refuses
// with ResourceExhausted again, with a growing wait. Every failure after RevertActor worked (the policy
// write, the resume, a failed wakeup probe) is execution.ErrRevertResume: the actor holds its snapshot.
func (d *Driver) RevertSandbox(ctx context.Context, workspace, name string) (execution.Sandbox, error) {
	const op = "RevertSandbox"
	k := key{workspace, name}
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	switch actor.GetStatus().GetState() {
	case ateapiv1.ActorState_ACTOR_STATE_CRASHED, ateapiv1.ActorState_ACTOR_STATE_RUNNING:
	default:
		return execution.Sandbox{}, fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not crashed or running", k, phaseOf(actor))
	}
	if len(actor.GetStatus().GetSnapshots()) == 0 {
		return execution.Sandbox{}, fail(op, execution.ErrNoSnapshot, "sandbox %s has no snapshot: a revert would give an empty home", k)
	}
	// The gate stops the driver's own calls to a guest that is going away.
	g := d.pauseGate(k)
	g.cancelAll()
	defer g.unpause()
	if err := d.retryExhausted(ctx, func() error {
		_, err := d.control.RevertActor(ctx, &ateapiv1.RevertActorRequest{Actor: ref(workspace, name)})
		return err
	}); err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	// From here the actor is SUSPENDED at its snapshot. Whatever fails next must not make the caller
	// delete it.
	if err := d.ensureEgressPolicy(ctx, op, k); err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrRevertResume, "sandbox %s was reverted and its egress policy was not written: %v", k, err)
	}
	var resumed *ateapiv1.Actor
	if err := d.retryExhausted(ctx, func() error {
		resp, err := d.control.ResumeActor(ctx, &ateapiv1.ResumeActorRequest{Actor: ref(workspace, name)})
		resumed = resp.GetActor()
		return err
	}); err != nil {
		// A failed wakeup probe lands here: the actor is CRASHED again, with the snapshot kept.
		return execution.Sandbox{}, fail(op, execution.ErrRevertResume, "sandbox %s was reverted and did not resume: %v", k, err)
	}
	d.setOwed(k, false)
	return sandboxFrom(workspace, resumed), nil
}

// retryExhausted runs call, and runs it again after a wait while it fails with ResourceExhausted (no
// worker has room now), up to exhaustedRetries times.
func (d *Driver) retryExhausted(ctx context.Context, call func() error) error {
	delay := d.cfg.PollInterval
	for attempt := 0; ; attempt++ {
		err := call()
		if status.Code(err) != codes.ResourceExhausted || attempt == exhaustedRetries {
			return err
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
	}
}
