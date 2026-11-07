// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The one place the guard reads old issue 350's surface: the org's leak-guard settings, the code-host scope
// (per code-host connection) and sinkclass.Classify. Everything else in the package sees a class and a threshold only, so a
// change to how the scope is keyed (org-wide or per connection) is an edit here.

// orgPolicy is what the guard needs of an org's settings for one call.
type orgPolicy struct {
	settings orgsettings.LeakGuard
}

func loadPolicy(ctx context.Context, q *store.Queries, org store.Org) (orgPolicy, error) {
	lg, err := orgsettings.LeakGuardOf(ctx, q, org.ID)
	if err != nil {
		return orgPolicy{}, err
	}
	return orgPolicy{settings: lg}, nil
}

// hosts is the org's fetch-trusted hosts: a fetched URL to one of them carries data only after its host.
func (p orgPolicy) hosts() sinkclass.HostTrust { return p.settings }

// threshold is the org's match threshold (0: the default).
func (p orgPolicy) threshold() int { return p.settings.MatchThreshold }

// classify returns the sink class of one call to a tool of srv under declaration d, from the
// call's address arguments only (ADR 0091 decision 3). A code-host tool's repository is looked up
// in the list of the code-host connection srv is linked to; an unlinked server ("") makes it
// external-high.
func (p orgPolicy) classify(srv store.McpServer, d sinkclass.Declaration, address map[string]string, now time.Time) sinkclass.Class {
	return sinkclass.Classify(d, address, srv.CodeHostConnectionID.String(), p.settings.Scope(), p.settings, now)
}

// thresholdOf is an org's match threshold (0: the default), for agentd's feed.
func thresholdOf(ctx context.Context, q *store.Queries, org pgtype.UUID) (int, error) {
	lg, err := orgsettings.LeakGuardOf(ctx, q, org)
	if err != nil {
		return 0, err
	}
	return orgPolicy{settings: lg}.threshold(), nil
}
