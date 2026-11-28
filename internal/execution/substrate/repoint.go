// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/harness"
)

// RepointSandbox implements execution.Repointer (issue #1482). A spec change makes a new template; the
// actor keeps its durable home when it names the new template through UpdateActor, so the reconciler
// need not delete the actor and restore the home from kopia. The steps are:
//
//  1. Check the invariants against the template the actor has now ([repointInvariants]). A breach is
//     execution.ErrRepointRefused, and nothing changed.
//  2. Create the new template (it can exist already: equal specs share a template).
//  3. Suspend a running actor with synchronous durability, so the home is saved.
//  4. UpdateActor to the new template. Substrate refuses some repoints (FailedPrecondition or
//     InvalidArgument): that is execution.ErrRepointRefused too, and the actor stays suspended with its
//     old template.
//  5. Write the egress policy and resume. A failure here is execution.ErrRepointResume: the actor names
//     the new template and holds its home, so the caller must not delete it.
//  6. Take one synchronous suspend and resume it (sealFirstBoot, shared with the first boot). The snapshot
//     that step 3 saved belongs to the old template; a revert of the actor (issue #1481) must not go back
//     to it. This step is best effort and logs its failure.
//
// The driver never deletes the old template: other actors can name it (see [Driver.DeleteUnusedTemplate]).
func (d *Driver) RepointSandbox(ctx context.Context, workspace, name string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	const op = "RepointSandbox"
	k := key{workspace, name}
	if err := checkSpec(op, spec); err != nil {
		return execution.Sandbox{}, err
	}
	if err := d.requireSigner(op); err != nil {
		return execution.Sandbox{}, err
	}
	if err := d.requireGatewayRoutes(op); err != nil {
		return execution.Sandbox{}, err
	}
	next, err := renderTemplate(workspace, spec, d.cfg)
	if err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrInvalid, "%v", err)
	}
	// No guest call of the driver starts while the actor changes. The gate nests with the suspend inside.
	g := d.pauseGate(k)
	defer g.unpause()

	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	switch actor.GetStatus().GetState() {
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED, ateapiv1.ActorState_ACTOR_STATE_RUNNING:
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDING, ateapiv1.ActorState_ACTOR_STATE_RESUMING:
		// A transient state: the next pass tries again.
		return execution.Sandbox{}, fail(op, execution.ErrUnavailable, "sandbox %s is %s: try again", k, phaseOf(actor))
	default:
		return execution.Sandbox{}, fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not suspended or running", k, phaseOf(actor))
	}
	if actor.GetActorTemplate().GetName() == next.GetMetadata().GetName() {
		return sandboxFrom(workspace, actor), nil
	}
	prev, err := d.control.GetActorTemplate(ctx, &ateapiv1.GetActorTemplateRequest{ActorTemplate: ref(workspace, actor.GetActorTemplate().GetName())})
	switch {
	case isCode(err, codeNotFound):
		// Without the old template the invariants cannot be checked: delete and restore is the safe way.
		return execution.Sandbox{}, fail(op, execution.ErrRepointRefused, "the template %q of sandbox %s is gone", actor.GetActorTemplate().GetName(), k)
	case err != nil:
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	if err := repointInvariants(prev, next, d.cfg); err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrRepointRefused, "sandbox %s: %v", k, err)
	}
	if _, err := d.control.CreateActorTemplate(ctx, &ateapiv1.CreateActorTemplateRequest{ActorTemplate: next}); err != nil && !isCode(err, codeAlreadyExists) {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	if actor.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_RUNNING {
		if _, _, err := d.suspend(ctx, op, workspace, name); err != nil {
			return execution.Sandbox{}, err
		}
		// The suspend changed the actor: read it for its version.
		if actor, err = d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)}); err != nil {
			return execution.Sandbox{}, execution.GRPCError(op, err)
		}
	}
	if st := actor.GetStatus().GetState(); st != ateapiv1.ActorState_ACTOR_STATE_SUSPENDED {
		// A late router request resumed the actor between the suspend and the read. The next pass tries again.
		return execution.Sandbox{}, fail(op, execution.ErrUnavailable, "sandbox %s is %s, not suspended, before the repoint", k, phaseOf(actor))
	}
	_, err = d.control.UpdateActor(ctx, &ateapiv1.UpdateActorRequest{Actor: &ateapiv1.Actor{
		Metadata:       &ateapiv1.ResourceMetadata{Atespace: workspace, Name: name, Uid: actor.GetMetadata().GetUid(), Version: actor.GetMetadata().GetVersion()},
		ActorTemplate:  ref(workspace, next.GetMetadata().GetName()),
		WorkerSelector: actor.GetWorkerSelector(), // a field the call does not name must not be cleared
	}})
	switch status.Code(err) {
	case codes.OK:
	case codes.FailedPrecondition, codes.InvalidArgument:
		return execution.Sandbox{}, fail(op, execution.ErrRepointRefused, "Substrate refused the repoint of sandbox %s: %v", k, err)
	default:
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	// From here the actor names the new template. Whatever fails next must not make the caller delete it.
	// Its snapshot belongs to the old template until a seal replaces it: the next resume takes the seal.
	d.setSealOwed(k, true)
	if err := d.ensureEgressPolicy(ctx, op, k); err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrRepointResume, "sandbox %s was repointed and its egress policy was not written: %v", k, err)
	}
	resp, err := d.retryResume(ctx, workspace, name)
	if err != nil {
		return execution.Sandbox{}, fail(op, execution.ErrRepointResume, "sandbox %s was repointed and did not resume: %v", k, err)
	}
	d.setOwed(k, false)
	if sb, ok := d.sealFirstBoot(ctx, op, k); ok {
		d.setSealOwed(k, false)
		return sb, nil
	}
	return sandboxFrom(workspace, resp.GetActor()), nil
}

var _ execution.Repointer = (*Driver)(nil)

// repointInvariants says why next cannot replace prev on an actor that keeps its home, or returns nil.
// The home of an actor lives in a durable directory. On gVisor its checkpoint is keyed by the container
// name (cmd/ateworker-gvisor/fscheckpoint.go), and Substrate checks none of this: a repoint that renames
// the container leaves the home EMPTY with no error (issue #1482). So these must stay equal:
//
//   - the container names, in order;
//   - the volumes, and the mounts of every container;
//   - the snapshot storage location;
//   - the isolation tier (ADR 0075) and the runtime class: a change of either changes the runtime and the
//     snapshot format.
//
// A change of any of them is a delete and a restore from kopia.
func repointInvariants(prev, next *ateapiv1.ActorTemplate, cfg Config) error {
	pc, nc := prev.GetContainers(), next.GetContainers()
	if len(pc) != len(nc) {
		return fmt.Errorf("the template has %d containers and the new one %d", len(pc), len(nc))
	}
	for i := range pc {
		if pc[i].GetName() != nc[i].GetName() {
			return fmt.Errorf("container %d is named %q and the new one %q: the home is keyed by the container name", i, pc[i].GetName(), nc[i].GetName())
		}
		if !equalMessages(pc[i].GetVolumeMounts(), nc[i].GetVolumeMounts()) {
			return fmt.Errorf("the volume mounts of container %q change", pc[i].GetName())
		}
	}
	if !equalMessages(prev.GetVolumes(), next.GetVolumes()) {
		return fmt.Errorf("the volumes change")
	}
	if p, n := prev.GetSnapshotConfig().GetStorageLocation(), next.GetSnapshotConfig().GetStorageLocation(); p != n {
		return fmt.Errorf("the snapshot storage location changes from %q to %q", p, n)
	}
	if p, n := tierOfTemplate(prev, cfg), tierOfTemplate(next, cfg); p != n {
		return fmt.Errorf("the isolation tier changes from %q to %q", p, n)
	}
	// A snapshot belongs to a runtime: two classes of one tier (kata-qemu and kata-clh) are not equal either.
	pc2, pok := prev.GetWorkerSelector().GetMatchLabels()[RuntimeClassSelector]
	nc2, nok := next.GetWorkerSelector().GetMatchLabels()[RuntimeClassSelector]
	if pok != nok || pc2 != nc2 {
		return fmt.Errorf("the runtime class changes from %q to %q", pc2, nc2)
	}
	if pok {
		if _, known := cfg.RuntimeClassTiers[nc2]; !known {
			return fmt.Errorf("the runtime class %q has no tier", nc2)
		}
	}
	return nil
}

// equalMessages reports whether two lists of messages are equal, in order.
func equalMessages[T proto.Message](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// tierOfTemplate is the isolation tier of a rendered template: the tier of the runtime class its worker
// selector names, or Config.DefaultTier without one. A class that is not in Config.RuntimeClassTiers has
// the empty tier.
func tierOfTemplate(t *ateapiv1.ActorTemplate, cfg Config) harness.Tier {
	class, ok := t.GetWorkerSelector().GetMatchLabels()[RuntimeClassSelector]
	if !ok {
		return cfg.DefaultTier
	}
	return cfg.RuntimeClassTiers[class]
}

// DeleteUnusedTemplate deletes a template that no actor of the atespace names, and fails with
// execution.ErrFailedPrecondition when any does (issue #1482, invariant 4). Substrate has no in-use
// check: a deleted template strands every actor that names it, and a RUNNING actor cannot suspend. No
// code path of the driver deletes a template except this one; the garbage collection of kt- templates
// waits for v0.2.
func (d *Driver) DeleteUnusedTemplate(ctx context.Context, workspace, template string) error {
	const op = "DeleteUnusedTemplate"
	if !IsDerivedTemplate(template) {
		return fail(op, execution.ErrInvalid, "template %q was not made by the driver", template)
	}
	var token string
	for {
		resp, err := d.control.ListActors(ctx, &ateapiv1.ListActorsRequest{Atespace: workspace, Options: &ateapiv1.ListOptions{PageToken: token}})
		if err != nil {
			return execution.GRPCError(op, err)
		}
		for _, a := range resp.GetActors() {
			if a.GetActorTemplate().GetName() == template {
				return fail(op, execution.ErrFailedPrecondition, "actor %s names template %q", a.GetMetadata().GetName(), template)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	// An actor made after the list names the template only if its caller created the actor with a template
	// that this call deletes: the driver creates a template just before the actor, and never deletes one
	// itself, so the window is the caller's to close (the v0.2 garbage collector runs on the leader).
	if _, err := d.control.DeleteActorTemplate(ctx, &ateapiv1.DeleteActorTemplateRequest{ActorTemplate: ref(workspace, template)}); err != nil && !isCode(err, codeNotFound) {
		return execution.GRPCError(op, err)
	}
	return nil
}
