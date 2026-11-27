// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"maps"
	"sync/atomic"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// Identifier finds the actor that an actor token speaks for. A [Gate] calls it once for each request,
// before the route check. Every error refuses the request; an Identifier never allows on an error.
//
// Two implementations exist, and the execution driver of the install selects one (ADR 0120, amendment
// for issue #1386, item 4):
//
//   - *actortoken.Verifier, the Ed25519 path of the OpenShell driver: the token itself names the org
//     and the agent. exit: #1386 PR B, after #1094.
//   - *SubstrateIdentifier: the token is Substrate's actor JWT, and Koinon's record of the actor
//     (the State) names the org and the agent.
type Identifier interface {
	Identify(token string) (actortoken.Actor, error)
}

// SubstrateTokenVerifier checks a Substrate actor JWT (*actortoken.SubstrateVerifier).
type SubstrateTokenVerifier interface {
	Verify(token string) (actortoken.SubstrateClaims, error)
}

// ActorTable is Koinon's record of the Agent Substrate actors, from the latest State revision
// (gateway.EgressState.ActorLookup). It is safe for concurrent use. The zero value knows no actor, so a
// gateway before its first State revision refuses every Substrate token.
type ActorTable struct {
	actors atomic.Pointer[map[actortoken.SubstrateActor]actortoken.Actor]
}

// Set replaces the record. Nil or an empty map removes every actor: each token is then refused. The
// table keeps its own copy.
func (t *ActorTable) Set(m map[actortoken.SubstrateActor]actortoken.Actor) {
	c := maps.Clone(m)
	t.actors.Store(&c)
}

// Len is the number of actors in the record.
func (t *ActorTable) Len() int {
	if p := t.actors.Load(); p != nil {
		return len(*p)
	}
	return 0
}

// Lookup returns the actor that k names. The key holds the UID, so an actor with the same workspace
// and name but another UID is not found.
func (t *ActorTable) Lookup(k actortoken.SubstrateActor) (actortoken.Actor, bool) {
	p := t.actors.Load()
	if p == nil {
		return actortoken.Actor{}, false
	}
	a, ok := (*p)[k]
	return a, ok
}

// SubstrateIdentifier identifies the actor of a Substrate actor JWT: the verifier checks the token, and
// the actor table, which follows the State stream, maps its workspace, actor name and actor UID to the
// org, agent and run (ADR 0120, amendment for issue #1386, item 5). The token never names the org or the
// agent. It reads the table on every call, so an actor that a State revision removes (a deleted or
// archived sandbox) loses its identity at once.
type SubstrateIdentifier struct {
	verifier SubstrateTokenVerifier
	actors   *ActorTable
}

// NewSubstrateIdentifier returns the identifier of v and actors. Both are required.
func NewSubstrateIdentifier(v SubstrateTokenVerifier, actors *ActorTable) (*SubstrateIdentifier, error) {
	if v == nil || actors == nil {
		return nil, errors.New("route: a Substrate identifier needs a verifier and an actor table")
	}
	return &SubstrateIdentifier{verifier: v, actors: actors}, nil
}

// Identify checks token and returns the recorded actor. A verifier error comes back as it is
// (actortoken.ErrUnavailable when the issuer keys cannot be read); a verified token whose actor is not
// in the table is actortoken.ErrUnknownActor.
func (s *SubstrateIdentifier) Identify(token string) (actortoken.Actor, error) {
	if s == nil {
		return actortoken.Actor{}, actortoken.ErrInvalid
	}
	c, err := s.verifier.Verify(token)
	if err != nil {
		return actortoken.Actor{}, err
	}
	a, ok := s.actors.Lookup(c.Key())
	if !ok || a.Validate() != nil {
		return actortoken.Actor{}, actortoken.ErrUnknownActor
	}
	return a, nil
}
