// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
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
// any answer of the front, NotFound included, proves that the front runs.
//
// A failed guest call is silence of the front (ErrFrontDown) only when the router answered and said that
// it could not reach the guest (frontUnreachable). An Unavailable that the router made itself (its
// ext_proc refused or shed the call) or that the client made (no connection to the router, a router
// restart) says nothing about the front and returns ErrUnavailable: a router outage must not revert every
// running sandbox. A deadline is silence only when the router connection is READY after the call and the
// control API read of the same call was fast (controlFastBound). Any other error is not a verdict on the
// front and is returned as it is.
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
		if status.Code(err) == codes.Unavailable {
			if !frontUnreachable(err) {
				return fail(op, execution.ErrUnavailable, "the guest call of sandbox %s did not reach the guest (%v): no verdict on the front", k, err)
			}
			return frontDown(actor, fail(op, execution.ErrFrontDown, "the guest front of sandbox %s does not answer: %v", k, err))
		}
		if !controlFast {
			return fail(op, execution.ErrUnavailable, "the guest call of sandbox %s ran into its deadline and the control API is slow too: no verdict on the front", k)
		}
		if d.routerState != nil && d.routerState() != connectivity.Ready {
			// A call on a connection that is still connecting waits for it and ends at the deadline.
			return fail(op, execution.ErrUnavailable, "the guest call of sandbox %s ran into its deadline and the router connection is %s: no verdict on the front", k, d.routerState())
		}
		return frontDown(actor, fail(op, execution.ErrFrontDown, "the guest front of sandbox %s does not answer in %s", k, d.cfg.FrontCheckTimeout))
	}
	return execution.GRPCError(op, err)
}

// frontDown adds the worker of the actor to an ErrFrontDown: both verdicts of CheckFront (the router's
// report and the deadline) carry it, so the reconciler's fleet breaker sees every one. The worker is the
// actor's worker assignment of the control API read in the same call: worker.name, else worker_pod
// (Substrate api/ateapi/v1/ateapi.proto:906, WorkerAssignment :987 and :1008).
func frontDown(actor *ateapiv1.Actor, err error) error {
	wa := actor.GetStatus().GetWorkerAssignment()
	worker := wa.GetWorker().GetName()
	if worker == "" {
		worker = wa.GetWorkerPod()
	}
	return &execution.FrontDownError{Worker: worker, Err: err}
}

// The two forms in which the router reports that it could not reach the guest. Every other Unavailable
// of a guest call comes from the router itself or from the client's connection to it.
const (
	// upstreamConnectError starts the local reply of the router's dataplane (Envoy) when the upstream
	// failed before it sent headers. In the default upstream CONNECT mode (Substrate cmd/atenet/internal/
	// router/cmd.go, --upstream-connect), atunnel answers the CONNECT with 502 when it cannot dial the
	// guest port (internal/atunnel/ingress/ingress.go, handleConnect), and Envoy answers a gRPC request
	// with this text as an Unavailable. The router's own refusals are ext_proc immediate responses with
	// other text (cmd/atenet/internal/router/ingress/errors.go, mapResumeError: "actor ... unavailable";
	// parkingFullErr: "... router at capacity").
	upstreamConnectError = "upstream connect error or disconnect/reset before headers"
	// forwardedBadGateway is how grpc-go reports the 502 of atunnel's reverse proxy (internal/atunnel/
	// ingress/ingress.go, newActorProxy ErrorHandler) that the router forwards with upstream CONNECT off: a
	// non-gRPC response with HTTP status 502. A router answer of its own is a 503 or a gRPC status.
	forwardedBadGateway = "unexpected HTTP status code received from server: 502 "
)

// frontUnreachable reports whether an Unavailable of a guest call is the router's report that it could
// not reach the guest, the only Unavailable that is a verdict on the front.
func frontUnreachable(err error) bool {
	s, ok := status.FromError(err)
	if !ok || s.Code() != codes.Unavailable {
		return false
	}
	m := s.Message()
	return strings.HasPrefix(m, upstreamConnectError) || strings.HasPrefix(m, forwardedBadGateway)
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
