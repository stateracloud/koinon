// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// MaxActorNameLength is the longest actor name that Substrate admits: a resource name is a DNS-1123 label (63
// characters), format k8s-short-name in api/ateapi/v1/ateapi.proto (ResourceMetadata.name, Substrate commit
// 20778a6d).
const MaxActorNameLength = 63

// NewActorName implements [execution.ActorNamer]: "<sandbox>-<16 lowercase hex from crypto/rand>" (issues #1386, #1485).
// Substrate signs the JWT of an actor with the actor name, so a name is new for every actor and never reused. The
// suffix has 64 bits, so that a name does not collide with a recently deleted one: the atenet-egress token cache
// is keyed by actor name, and a reused name got the UID of a deleted actor (lab, 2026-11-27). The
// result is longer than [MaxActorNameLength] when the sandbox name is longer than 46 characters; the driver does not
// shorten it, and Substrate then refuses the create with InvalidArgument (the Koinon sandbox names are 19 characters
// or fewer). A random name is not the guard: the driver also refuses every name that its [ActorNameLedger] holds
// (issue #1092).
func (d *Driver) NewActorName(sandbox string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("substrate: no random bytes: " + err.Error()) // crypto/rand does not fail on a supported platform
	}
	return sandbox + "-" + hex.EncodeToString(b[:])
}

// ActorNameLedger is the durable record of the actor names that a driver sent to CreateActor, per atespace (issue
// #1092). Substrate v1.0.1 forgets a deleted actor, but atenet-egress does not: it caches an actor JWT by actor name
// until a third of its lifetime is left (cmd/atenet/internal/router/egress/actorjwt/actorjwt.go:32-34,90-92), and
// the egress gateway caches a policy by name for 10 s (upstream #3267). A new actor under a used name can so get
// the token of the deleted actor (lab, rc.33: about 1.5 s after the delete). The Koinon gateway refuses that token
// by the actor UID (403 unknown_actor, internal/gateway/route/identity.go:94), so the harm is lost egress for the
// new actor, not a leak of identity. The ledger is a second layer next to the random name of [Driver.NewActorName]:
// the driver claims each name here before CreateActor and refuses a name that was claimed before
// ([execution.ErrNameReused]).
//
// The record must outlive a restart of the driver and be shared by every driver replica: production uses the
// Koinon database (package actornames, table substrate_actor_names). A record is never removed.
type ActorNameLedger interface {
	// ClaimActorName records that the driver sends the actor name in atespace to CreateActor. It returns true
	// when this call recorded the name, and false when the name was recorded before, by any driver, also before
	// a restart.
	ClaimActorName(ctx context.Context, atespace, name string) (bool, error)
}

// claimActorName claims the name of k in the ledger before a create. It returns nil, nil when the name is new.
// When the name was claimed before, it returns the live actor of that name (the same create came again: the caller
// returns that actor, or refuses it as a restore target), or [execution.ErrNameReused] when no live actor has the
// name: the actor was deleted, is being deleted, or a create that may have made it failed. The check fails closed:
// without a ledger, or with a ledger that does not answer, the driver creates nothing.
func (d *Driver) claimActorName(ctx context.Context, op string, k key) (*ateapiv1.Actor, error) {
	if d.cfg.ActorNames == nil {
		return nil, fail(op, execution.ErrFailedPrecondition, "no actor name ledger: the driver cannot prove that %s was never used (issue #1092)", k)
	}
	fresh, err := d.cfg.ActorNames.ClaimActorName(ctx, k.atespace, k.actor)
	if err != nil {
		return nil, fail(op, execution.ErrUnavailable, "claim the actor name %s: %v", k, err)
	}
	if fresh {
		return nil, nil
	}
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
	if isCode(err, codeNotFound) {
		return nil, nameReused(op, k)
	}
	if err != nil {
		return nil, execution.GRPCError(op, err)
	}
	if actor.GetStatus().GetState() == ateapiv1.ActorState_ACTOR_STATE_DELETING {
		return nil, nameReused(op, k)
	}
	return actor, nil
}

func nameReused(op string, k key) error {
	return fail(op, execution.ErrNameReused, "the actor name %s was used by an earlier actor, and atenet-egress can still hold that actor's token: no actor is created under it (issue #1092)", k)
}
