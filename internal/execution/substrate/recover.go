// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	ateenvv1alpha "github.com/stateracloud/koinon/internal/gen/substrate/ateenv/v1alpha"
)

// The crash and liveness path of issue #1481. A Substrate actor does not signal when pid 1 (the guest
// front) exits: it stays RUNNING. The driver gives the reconciler three calls: CheckFront finds that
// the front is silent, FaultSandbox forces the crash, and RevertSandbox goes back to the last
// snapshot. RevertSandbox keeps the home (about 0.1 s), where a delete and a create gives an empty one.

var _ execution.Reverter = (*Driver)(nil)

// frontCheckTimeout is how long CheckFront waits for the front to answer. The front is a local
// process of the guest: a longer wait is a front that does not run.
const frontCheckTimeout = 5 * time.Second

// exhaustedRetries is how many times a call that Substrate refused with ResourceExhausted is sent again.
const exhaustedRetries = 4

// CheckFront implements execution.Reverter. It reads the actor first with the control API, which never
// resumes it, and calls the guest only when the actor runs. It asks for a process that does not exist:
// any answer of the front, NotFound included, proves that the front runs. A call that ends with
// Unavailable or a deadline is no answer. Any other error is not a verdict on the front and is returned
// as it is.
func (d *Driver) CheckFront(ctx context.Context, workspace, name string) error {
	const op = "CheckFront"
	k := key{workspace, name}
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.GRPCError(op, err)
	}
	if st := actor.GetStatus().GetState(); st != ateapiv1.ActorState_ACTOR_STATE_RUNNING {
		return fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not running", k, phaseOf(actor))
	}
	cctx, cancel := context.WithTimeout(ctx, frontCheckTimeout)
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
		return fail(op, execution.ErrFrontDown, "the guest front of sandbox %s does not answer: %v", k, err)
	}
	return execution.GRPCError(op, err)
}

// FaultSandbox implements execution.Reverter: it sends SuspendActor, which a running actor with a dead
// front does not survive as a snapshot. The router is not used, because a call through it resumes a
// suspended actor. It does not wait.
func (d *Driver) FaultSandbox(ctx context.Context, workspace, name string) error {
	_, err := d.control.SuspendActor(ctx, &ateapiv1.SuspendActorRequest{
		Actor:   ref(workspace, name),
		Options: &ateapiv1.SuspendOptions{Mode: ateapiv1.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	})
	return execution.GRPCError("FaultSandbox", err)
}

// RevertSandbox implements execution.Reverter. The actor must be CRASHED. It fails with
// execution.ErrNoSnapshot when the actor never suspended, because the revert would give an empty home.
// It sends the egress policy again (resume does), and it sends a call that Substrate refuses with
// ResourceExhausted again, with a growing wait.
func (d *Driver) RevertSandbox(ctx context.Context, workspace, name string) (execution.RevertResult, error) {
	const op = "RevertSandbox"
	k := key{workspace, name}
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.RevertResult{}, execution.GRPCError(op, err)
	}
	if st := actor.GetStatus().GetState(); st != ateapiv1.ActorState_ACTOR_STATE_CRASHED {
		return execution.RevertResult{}, fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not crashed", k, phaseOf(actor))
	}
	if len(actor.GetStatus().GetSnapshots()) == 0 {
		return execution.RevertResult{}, fail(op, execution.ErrNoSnapshot, "sandbox %s has no snapshot: a revert would give an empty home", k)
	}
	// The gate stops the driver's own calls to a guest that is going away.
	g := d.pauseGate(k)
	g.cancelAll()
	defer g.unpause()
	if err := d.retryExhausted(ctx, func() error {
		_, err := d.control.RevertActor(ctx, &ateapiv1.RevertActorRequest{Actor: ref(workspace, name)})
		return err
	}); err != nil {
		return execution.RevertResult{}, execution.GRPCError(op, err)
	}
	if err := d.ensureEgressPolicy(ctx, op, k); err != nil {
		return execution.RevertResult{}, err
	}
	var resumed *ateapiv1.Actor
	if err := d.retryExhausted(ctx, func() error {
		resp, err := d.control.ResumeActor(ctx, &ateapiv1.ResumeActorRequest{Actor: ref(workspace, name)})
		resumed = resp.GetActor()
		return err
	}); err != nil {
		return execution.RevertResult{}, execution.GRPCError(op, err)
	}
	d.setOwed(k, false)
	d.mu.Lock()
	at := d.snapAt[k]
	d.mu.Unlock()
	return execution.RevertResult{Sandbox: sandboxFrom(workspace, resumed), SnapshotAt: at}, nil
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

// CrashKind says what a crash message is about. Koinon decides by the actor state and the message and
// never by the gRPC code of the call: a failed wakeup probe gives the code Internal for many reasons.
type CrashKind string

// Crash kinds.
const (
	// CrashProbe: the wakeup probe of the front failed.
	CrashProbe CrashKind = "probe"
	// CrashImage: the image could not be pulled.
	CrashImage CrashKind = "image"
	// CrashOther: any other crash.
	CrashOther CrashKind = "other"
)

// ClassifyCrash classifies a sandbox by its phase and its message. Only a sandbox in phase Error has a
// crash kind; any other phase gives the empty kind.
func ClassifyCrash(sb execution.Sandbox) CrashKind {
	if sb.Phase != execution.PhaseError {
		return ""
	}
	m := strings.ToLower(sb.Message)
	switch {
	case strings.Contains(m, "probe"):
		return CrashProbe
	case execution.ClassifySandboxWait(sb.Reason, sb.Message) == execution.WaitCauseImagePull:
		return CrashImage
	}
	return CrashOther
}
