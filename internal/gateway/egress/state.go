// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// Merge returns the table of the platform routes and the org routes from Koinon state (ADR 0097
// step 2, #1086 part B). An org route must name its org and must have a name with a "/", so it can
// never pose as a platform route or take a platform route's name. Any fault refuses the whole set of
// org routes; the caller then serves the platform routes alone, so a bad org rule closes the org
// routes and never opens one (ADR 0120 2.2).
func Merge(platform *route.Table, org []route.Route) (*route.Table, error) {
	for _, r := range org {
		if r.Org == "" {
			return nil, fmt.Errorf("egress: org route %q names no org", r.Name)
		}
		if !strings.Contains(r.Name, "/") {
			return nil, fmt.Errorf("egress: org route %q: the name of an org route has a \"/\"", r.Name)
		}
	}
	all := append(platform.Routes(), org...)
	t, err := route.Compile(all)
	if err != nil {
		return nil, fmt.Errorf("egress: the org routes: %w", err)
	}
	return t, nil
}

// StateRoutes applies the org routes of each State revision on top of a fixed platform table. A set
// that Merge refuses is never served in part: the gateway then serves the platform routes alone, and
// ApplyEgress returns the fault so the caller logs and counts it.
type StateRoutes struct {
	Gateway *Gateway
	// Platform is the table of the platform routes file; nil when there is no file.
	Platform *route.Table
	// Actors, when set, gets the Substrate actors of each revision (EgressState.ActorLookup): the
	// table the Substrate identifier reads (ADR 0120, amendment for issue #1386, item 5). Nil on the
	// Ed25519 path, where the token names the actor.
	Actors *route.ActorTable
}

// ApplyEgress merges the org routes of e with the platform routes and installs the result with the
// credentials of e. A fault in the routes or the credentials installs the platform routes alone. The
// actors of e replace the actor table first, whatever the routes do, so an actor that the revision
// removed loses its identity; a fault in the actors empties the table (every Substrate token is then
// refused) and is returned with any route fault.
func (s StateRoutes) ApplyEgress(_ context.Context, e gateway.EgressState) error {
	var actorErr error
	if s.Actors != nil {
		m, err := e.ActorLookup()
		if err != nil {
			m, actorErr = nil, fmt.Errorf("egress: the actors: %w", err)
		}
		s.Actors.Set(m)
	}
	t, err := Merge(s.Platform, e.EgressRoutes)
	if err != nil {
		s.Gateway.Apply(s.Platform)
		return errors.Join(actorErr, err)
	}
	creds, err := route.CheckCredentials(e.EgressRoutes, e.EgressCredentials)
	if err != nil {
		s.Gateway.Apply(s.Platform)
		return errors.Join(actorErr, fmt.Errorf("egress: the org credentials: %w", err))
	}
	s.Gateway.ApplyWithCredentials(t, creds)
	return actorErr
}
