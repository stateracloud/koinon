// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// The actor JWT on the model and MCP listeners (issue #1489, ADR 0120 amendment of 2026-11-27).
//
// On Agent Substrate the guest holds no gateway key. It sends a fixed public placeholder
// (actortoken.Placeholder) as its bearer token, and atenet-egress, outside the guest, replaces the
// Authorization header with the actor's JWT, minted for the audience of the listener. The listener
// checks the JWT (ActorIdentifier) and finds the agent from Koinon's own record of the actor. A
// bearer that starts with KeyPrefix is a gateway key: Koinon's own callers (koinon-api, the
// reconciler's probes) still use one. Any other bearer is an actor JWT, and there is no fallback to
// the key path: the placeholder, an empty bearer and a token that does not verify get 401.

// ActorIdentifier finds the actor that a Substrate actor JWT speaks for (*route.SubstrateIdentifier,
// built with the verifier of the listener's audience).
type ActorIdentifier interface {
	Identify(token string) (actortoken.Actor, error)
}

// ActorRefusal is the answer to a bearer that is not accepted as an actor JWT.
type ActorRefusal struct {
	// Status is 401 for a missing or bad token, 403 for a verified token whose actor Koinon does not
	// know, and 503 when the issuer keys are not available.
	Status int
	// Text is the response body. It never holds the token.
	Text string
	// Reason is a fixed, low-cardinality reason for a log field (actortoken.Reason).
	Reason string
	// Final says the refusal carries the do-not-retry marker. A 503 does not: the keys can come back.
	Final bool
}

// IsGatewayKey reports whether bearer is a gateway key and not an actor JWT.
func IsGatewayKey(bearer string) bool { return strings.HasPrefix(bearer, KeyPrefix) }

// IdentifyActor checks bearer as an actor JWT with id. It returns the agent of the actor, or the
// refusal. bearer is the value after "Bearer ", trimmed; "" means an empty bearer.
func IdentifyActor(id ActorIdentifier, bearer string) (AgentRef, *ActorRefusal) {
	if bearer == "" {
		return AgentRef{}, &ActorRefusal{Status: http.StatusUnauthorized, Text: "refused: no actor token", Reason: "no_token", Final: true}
	}
	a, err := id.Identify(bearer)
	switch {
	case err == nil:
		return AgentRef{Org: a.Org, Name: a.Agent}, nil
	case errors.Is(err, actortoken.ErrUnavailable):
		return AgentRef{}, &ActorRefusal{Status: http.StatusServiceUnavailable, Text: "refused: the actor token issuer keys are not available", Reason: actortoken.Reason(err)}
	case errors.Is(err, actortoken.ErrUnknownActor):
		return AgentRef{}, &ActorRefusal{Status: http.StatusForbidden, Text: "refused: the actor is not known to the gateway", Reason: actortoken.Reason(err), Final: true}
	default:
		return AgentRef{}, &ActorRefusal{Status: http.StatusUnauthorized, Text: "refused: the actor token is not valid", Reason: actortoken.Reason(err), Final: true}
	}
}
