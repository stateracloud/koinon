// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

// Sandbox actors (issue #1386, ADR 0120 amendment of 2026-11-26). On Agent Substrate a sandbox runs as an
// actor, and Substrate signs a JWT for it that names the workspace (atespace), the actor name and the actor
// UID. The gateway finds the org and the agent of the token in Koinon's State (EgressActors), never in the
// token. So a name must never come back for another actor: each new actor gets a new name (the driver's
// ActorNamer), recorded before the create. The UID that Substrate gives the actor is recorded when the
// reconciler first sees it, and the next State revision carries the actor. A run then waits for that
// revision, as it waits for its forge credential (forgegate.go).

// nameNewActor records a new actor name for w, before each create, when the backend names actors. The
// caller found no actor under the current name. That name can still have been used: a create can succeed
// (or fail in a way that does not say whether it created the actor) and the actor can go before a pass
// records its UID. atenet-egress keeps tokens by actor name, so a new actor under that name could get a
// token of the old one. So each create gets a new name, and the reconciler never sends a name to
// CreateSandbox twice.
//
// The driver drops the gateway access it recorded for the old name, which no sandbox uses again, and the
// new name gets its access. It reports whether it named a new actor; the caller renders the spec again.
func (r *Reconciler) nameNewActor(ctx context.Context, w *work, ws string, access execution.GatewayAccess) (bool, error) {
	n, ok := execution.ActorNamerOf(r.backend)
	if !ok {
		return false, nil // OpenShell: the sandbox name is the name of every driver call
	}
	old := w.sb.ActorRef()
	row, err := r.q.SetAgentSandboxActorName(ctx, store.SetAgentSandboxActorNameParams{
		OrgID: w.org.ID, AgentID: w.agent.ID, ActorName: n.NewActorName(w.sb.SandboxName),
	})
	if err != nil {
		return false, fmt.Errorf("record the name of a new sandbox actor: %w", err)
	}
	w.sb = row
	r.log.Info("named a new sandbox actor", w.logAttrs()...)
	if s := execution.GatewayAccessSetterOf(r.backend); s != nil {
		s.ForgetGatewayAccess(ws, old)
		if err := s.SetGatewayAccess(ctx, ws, row.ActorName.String, access); err != nil {
			return true, err
		}
	}
	return true, nil
}

// recordActor records the UID of the named actor that the backend reports for w, when it is new. The
// store then resets the agent's forge row, so a run waits for the State revision that carries the actor,
// and the reconciler wakes to publish that revision. A legacy actor under the sandbox name (no actor name
// recorded) gets no UID: the reconciler recreates it (unnamedActor), and the new actor gets a name.
func (r *Reconciler) recordActor(ctx context.Context, w *work, sb execution.Sandbox) error {
	if _, ok := execution.ActorNamerOf(r.backend); !ok || sb.ID == "" || !w.sb.ActorName.Valid || sb.Name != w.sb.ActorName.String {
		return nil
	}
	if w.sb.ActorUid.Valid && w.sb.ActorUid.String == sb.ID {
		return nil
	}
	row, err := r.q.SetAgentSandboxActorUID(ctx, store.SetAgentSandboxActorUIDParams{
		OrgID: w.org.ID, AgentID: w.agent.ID, ActorName: sb.Name, ActorUid: sb.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The row names another actor now, or has this UID already: nothing to record.
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the UID of the sandbox actor: %w", err)
	}
	w.sb = row
	r.log.Info("recorded the UID of a new sandbox actor", append(w.logAttrs(), "actor_uid", sb.ID)...)
	if r.pool != nil {
		notifySelf(r.pool, r.log)
	}
	return nil
}

// actorOf is the Substrate actor of w that the State carries: one with a name and a recorded UID, on a
// backend that names actors, of a sandbox that is not deleted or being deleted (ADR 0120 amendment for
// #1386, item 5). It is nil otherwise. The row keeps the name and the UID of a deleted actor until the next
// actor is named, but a token for that actor must not identify the agent.
func (r *Reconciler) actorOf(w *work) *gatewayplan.Actor {
	if _, ok := execution.ActorNamerOf(r.backend); !ok || !w.sb.ActorName.Valid || !w.sb.ActorUid.Valid || w.sb.ActorUid.String == "" {
		return nil
	}
	if w.sb.State == store.SandboxStateDeleting || w.sb.State == store.SandboxStateDeleted {
		return nil
	}
	return &gatewayplan.Actor{Atespace: w.sb.Workspace, Name: w.sb.ActorName.String, UID: w.sb.ActorUid.String}
}

// noteActors records, for each actor that revision rev holds, the first revision that held it, as
// noteForge does for the forge credentials.
func (r *Reconciler) noteActors(actors []gateway.EgressActor, rev int64) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	next := make(map[gatewayplan.Actor]int64, len(actors))
	for _, a := range actors {
		k := gatewayplan.Actor{Atespace: a.Atespace, Name: a.Name, UID: a.UID}
		if old, ok := r.actorRevs[k]; ok && old <= rev {
			next[k] = old
		} else {
			next[k] = rev
		}
	}
	r.actorRevs = next
}

// actorRev is the first stored revision that holds the actor a, if one does.
func (r *Reconciler) actorRev(a gatewayplan.Actor) (int64, bool) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	rev, ok := r.actorRevs[a]
	return rev, ok
}
