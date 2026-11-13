// SPDX-License-Identifier: Apache-2.0

package runlimits

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// Continuation is what bounds the continuation runs of one task (issue #99, ADR 0081 amendment
// 2026-11-12): when a run hits its time limit, the task gets another run while it has had fewer
// than Max continuations and has spent less than TaskSpend.
type Continuation struct {
	// Max is the continuation runs a task gets; 0 means none.
	Max int
	// TaskSpend is the task's spend (all its runs, by the ledger's settled cost) at which no further
	// continuation starts.
	TaskSpend money.USD
}

// ContinuationOverride is an admin's per-agent override of the continuation limits. A nil
// MaxContinuations follows the template; a zero TaskSpend follows the org's default.
type ContinuationOverride struct {
	MaxContinuations *int
	TaskSpend        money.USD
}

// IsZero reports whether the override sets nothing.
func (o ContinuationOverride) IsZero() bool { return o.MaxContinuations == nil && o.TaskSpend == 0 }

// GetContinuationOverride reads the agent's override; the zero value when it has none.
func GetContinuationOverride(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (ContinuationOverride, error) {
	row, err := q.GetAgentContinuationLimits(ctx, store.GetAgentContinuationLimitsParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return ContinuationOverride{}, nil
	} else if err != nil {
		return ContinuationOverride{}, err
	}
	var o ContinuationOverride
	if row.MaxContinuations.Valid {
		n := int(row.MaxContinuations.Int32)
		o.MaxContinuations = &n
	}
	if row.TaskSpendUsd != "" {
		if o.TaskSpend, err = money.ParseNumeric(row.TaskSpendUsd); err != nil {
			return ContinuationOverride{}, fmt.Errorf("runlimits: task spend cap: %w", err)
		}
	}
	return o, nil
}

// ContinuationFor resolves the continuation limits of an agent: the agent's override, over the
// template's count (cfg) and the org's default cap (org).
func ContinuationFor(ctx context.Context, q *store.Queries, orgID, agent pgtype.UUID, cfg agentconfig.Config) (Continuation, error) {
	set, err := orgsettings.Get(ctx, q, orgID, orgsettings.Defaults())
	if err != nil {
		return Continuation{}, fmt.Errorf("read the org's settings: %w", err)
	}
	o, err := GetContinuationOverride(ctx, q, orgID, agent)
	if err != nil {
		return Continuation{}, fmt.Errorf("read the agent's continuation limits: %w", err)
	}
	c := Continuation{Max: cfg.MaxContinuations(), TaskSpend: set.ContinuationTaskSpend}
	if o.MaxContinuations != nil {
		c.Max = *o.MaxContinuations
	}
	if o.TaskSpend > 0 {
		c.TaskSpend = o.TaskSpend
	}
	return c, nil
}

// ReadTaskSpend sums the settled cost of the task's runs.
func ReadTaskSpend(ctx context.Context, q *store.Queries, org, task pgtype.UUID) (money.USD, error) {
	s, err := q.GetTaskSpend(ctx, store.GetTaskSpendParams{OrgID: org, TaskID: task})
	if err != nil {
		return 0, err
	}
	return money.ParseNumeric(s)
}
