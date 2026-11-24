// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"fmt"
	"strings"

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
}

// ApplyEgress merges org with the platform routes and installs the result.
func (s StateRoutes) ApplyEgress(_ context.Context, org []route.Route) error {
	t, err := Merge(s.Platform, org)
	if err != nil {
		s.Gateway.Apply(s.Platform)
		return err
	}
	s.Gateway.Apply(t)
	return nil
}
