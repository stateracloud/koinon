// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

const (
	codeNotFound           = codes.NotFound
	codeAlreadyExists      = codes.AlreadyExists
	codeAborted            = codes.Aborted
	codeFailedPrecondition = codes.FailedPrecondition
)

func isCode(err error, c codes.Code) bool { return err != nil && status.Code(err) == c }

// phaseOf maps an actor's state to a sandbox phase.
//
//	RESUMING, no snapshot yet  Provisioning (a new actor boots from its template)
//	RESUMING, REVERTING        Starting
//	RUNNING                    Ready
//	SUSPENDING                 Stopping
//	SUSPENDED                  Suspended (Substrate has no separate stopped state)
//	CRASHED                    Error
//	DELETING                   Deleting
func phaseOf(a *ateapiv1.Actor) execution.Phase {
	st := a.GetStatus()
	switch st.GetState() {
	case ateapiv1.ActorState_ACTOR_STATE_RESUMING:
		if len(st.GetSnapshots()) == 0 {
			return execution.PhaseProvisioning
		}
		return execution.PhaseStarting
	case ateapiv1.ActorState_ACTOR_STATE_REVERTING:
		return execution.PhaseStarting
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
		return execution.PhaseReady
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDING:
		return execution.PhaseStopping
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
		return execution.PhaseSuspended
	case ateapiv1.ActorState_ACTOR_STATE_CRASHED:
		return execution.PhaseError
	case ateapiv1.ActorState_ACTOR_STATE_DELETING:
		return execution.PhaseDeleting
	}
	return execution.PhaseUnknown
}

// sandboxFrom renders an actor as a sandbox. Substrate keeps no labels on an actor, and no network
// policy, so Labels and Enforcement stay empty.
func sandboxFrom(workspace string, a *ateapiv1.Actor) execution.Sandbox {
	sb := execution.Sandbox{
		Name:      a.GetMetadata().GetName(),
		ID:        a.GetMetadata().GetUid(),
		Workspace: workspace,
		Phase:     phaseOf(a),
		// The template name is a hash of the template's content: the durable identity of the spec.
		SpecID: a.GetActorTemplate().GetName(),
	}
	if crash := a.GetStatus().GetCrash(); crash != nil && a.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_CRASHED {
		sb.Reason = "ActorCrashed"
		sb.Message = execution.TruncateWaitDetail(crash.GetMessage())
	}
	return sb
}

// Observation is what the actor-state observer reports about a sandbox that is not ready. It
// replaces the pod observer of ADR 0078: Substrate has no pods the reconciler may read, so the
// actor's own status is the evidence.
type Observation struct {
	Sandbox execution.Sandbox
	// Cause names why the sandbox is not ready, from the fixed set of [execution.SandboxWaitCause]
	// (a metric label). It is empty for a ready or deliberately suspended sandbox.
	Cause execution.SandboxWaitCause
	// Worker is the worker pod the actor is assigned to, for the operator's eyes only: it is a name
	// and never a metric label (ADR 0023).
	Worker string
	// Since is when the actor's record last changed.
	Since time.Time
}

// Observe reads an actor and explains it. It uses the control API only, so it never resumes the
// actor, and is safe to call while the actor is suspended. A crashed actor's message goes through
// [execution.ClassifySandboxWait], so an image that cannot be pulled is image_pull and not unknown.
func (d *Driver) Observe(ctx context.Context, workspace, name string) (Observation, error) {
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		if isCode(err, codeNotFound) {
			d.forget(key{workspace, name})
		}
		return Observation{}, execution.GRPCError("Observe", err)
	}
	o := Observation{
		Sandbox: sandboxFrom(workspace, actor),
		Worker:  actor.GetStatus().GetWorkerAssignment().GetWorkerPod(),
	}
	if t := actor.GetMetadata().GetUpdateTime(); t != nil {
		o.Since = t.AsTime()
	}
	switch o.Sandbox.Phase {
	case execution.PhaseError, execution.PhaseProvisioning, execution.PhaseStarting:
		if o.Sandbox.Phase == execution.PhaseError || actor.GetStatus().GetCrash() != nil {
			o.Cause = execution.ClassifySandboxWait(o.Sandbox.Reason, o.Sandbox.Message)
		} else if actor.GetStatus().GetWorkerAssignment().GetWorkerPod() == "" {
			// Waiting without a worker: the scheduler found none (no ready pool, no capacity).
			o.Cause = execution.WaitCauseNoNodes
		} else {
			o.Cause = execution.WaitCauseUnknown
		}
	}
	return o, nil
}

// WaitPhase polls the actor with the control API until its phase is want, or ctx ends. It is for
// callers that must not touch the router while they wait.
func (d *Driver) WaitPhase(ctx context.Context, workspace, name string, want execution.Phase) (execution.Sandbox, error) {
	delay := d.cfg.PollInterval
	for {
		sb, err := d.GetSandbox(ctx, workspace, name)
		if err != nil || sb.Phase == want {
			return sb, err
		}
		if sb.Phase == execution.PhaseError || sb.Phase == execution.PhaseDeleting {
			return sb, fail("WaitPhase", execution.ErrFailedPrecondition, "sandbox %s/%s is %s: %s", workspace, name, sb.Phase, sb.Message)
		}
		if err := sleep(ctx, delay); err != nil {
			return sb, err
		}
		delay = min(delay*2, 8*d.cfg.PollInterval)
	}
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
