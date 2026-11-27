// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

// A run of an agent with a Substrate actor waits for the State revision that carries the actor, and for the
// one that carries its forge credential: the later of the two (issue #1386).
func TestForgeWantWaitsForTheActorRevision(t *testing.T) {
	r := &Reconciler{}
	const cred = "org/acme/agent/a1/codehost"
	actor := gatewayplan.Actor{Atespace: "ws-acme", Name: "ka-a1-0a1b2c3d", UID: "uid-1"}
	w := &work{
		agent:        store.Agent{Status: store.AgentStatusActive},
		forgeExpects: true,
		forge:        &gatewayplan.ForgeCredential{Name: cred},
		actor:        &actor,
	}
	r.noteForge([]route.Credential{{Name: cred}}, 4)

	if row, ok := r.forgeWant(w); !ok || row.rev != 0 {
		t.Fatalf("no revision holds the actor yet: want rev 0, got %+v %v", row, ok)
	}

	held := []gateway.EgressActor{{Atespace: actor.Atespace, Name: actor.Name, UID: actor.UID, Org: "acme", Agent: "a1"}}
	r.noteActors(held, 6)
	r.noteForge([]route.Credential{{Name: cred}}, 6)
	if row, _ := r.forgeWant(w); row.rev != 6 {
		t.Fatalf("want the actor revision 6, got %d", row.rev)
	}

	r.noteActors(held, 9)
	if row, _ := r.forgeWant(w); row.rev != 6 {
		t.Fatalf("a later revision keeps the first one that held the actor: got %d", row.rev)
	}

	next := actor
	next.UID = "uid-2"
	w.actor = &next
	if row, _ := r.forgeWant(w); row.rev != 0 {
		t.Fatalf("a new UID waits for a revision that holds it: got %d", row.rev)
	}

	w.actor = nil
	if row, _ := r.forgeWant(w); row.rev != 4 {
		t.Fatalf("without an actor the credential revision counts: got %d", row.rev)
	}

	r.backend = namingBackend{}
	if row, ok := r.forgeWant(w); !ok || row.rev != 0 {
		t.Fatalf("a backend that names actors waits for the actor: want rev 0, got %+v %v", row, ok)
	}
}

// namingBackend is a backend that names actors, as the Substrate driver does. Only NewActorName is used.
type namingBackend struct{ execution.Backend }

func (namingBackend) NewActorName(sandbox string) string { return sandbox + "-0a1b2c3d" }
