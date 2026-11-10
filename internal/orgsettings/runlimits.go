// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The org's run limits (DB2, ADR 0066): the runs it may have starting or running at once, and its
// share of the DBOS queue for trigger work. They are the three columns the table bounds with a
// CHECK; the bounds below repeat them so a bad value is refused with a message and not a constraint
// name. max_concurrent_runs is read in run_claimable at every claim, so a change reaches runs that
// are already queued, a restored backlog included (issue #462).
const (
	MinMaxConcurrentRuns, MaxMaxConcurrentRuns       = 1, 10000
	MinTriggerConcurrency, MaxTriggerConcurrency     = 1, 1000
	MinTriggerRatePerMinute, MaxTriggerRatePerMinute = 1, 100000
	runLimitsSetAction                               = "org.run_limits.set"
)

// RunLimits are the org's effective run limits.
type RunLimits struct {
	MaxConcurrentRuns, TriggerConcurrency, TriggerRatePerMinute int
}

// RunLimitsPatch changes some of the org's run limits. A nil field stays as it is; a field that
// points at 0 returns to the platform default; any other value is the new limit.
type RunLimitsPatch struct {
	MaxConcurrentRuns, TriggerConcurrency, TriggerRatePerMinute *int
}

func checkLimit(name string, n *int, lo, hi int) error {
	if n == nil || *n == 0 {
		return nil
	}
	if *n < lo || *n > hi {
		return &InvalidError{Msg: fmt.Sprintf("%s must be %d to %d (0 is the platform default)", name, lo, hi)}
	}
	return nil
}

// Validate checks the patch against the table's bounds.
func (p RunLimitsPatch) Validate() error {
	return errors.Join(
		checkLimit("max concurrent runs", p.MaxConcurrentRuns, MinMaxConcurrentRuns, MaxMaxConcurrentRuns),
		checkLimit("trigger concurrency", p.TriggerConcurrency, MinTriggerConcurrency, MaxTriggerConcurrency),
		checkLimit("trigger rate per minute", p.TriggerRatePerMinute, MinTriggerRatePerMinute, MaxTriggerRatePerMinute),
	)
}

func runLimitsOf(s Settings) RunLimits {
	return RunLimits{MaxConcurrentRuns: s.MaxConcurrentRuns, TriggerConcurrency: s.TriggerConcurrency, TriggerRatePerMinute: s.TriggerRatePerMinute}
}

// RunLimits returns the org's effective run limits.
func (s *Service) RunLimits(ctx context.Context, org pgtype.UUID) (RunLimits, error) {
	st, err := Get(ctx, store.New(s.pool), org, Defaults())
	return runLimitsOf(st), err
}

// merge applies one patched field to the stored override: nil keeps it, 0 clears it.
func merge(cur pgtype.Int4, patch *int) pgtype.Int4 {
	switch {
	case patch == nil:
		return cur
	case *patch == 0:
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*patch), Valid: true} //nolint:gosec // bounded by Validate
}

// SetRunLimits applies the patch to the org's run limits and audits a change as org.run_limits.set,
// with the effective limits before and after. Other settings of the org stay. An org that does not
// exist is ErrNotFound.
func (s *Service) SetRunLimits(ctx context.Context, org pgtype.UUID, actor audit.Actor, p RunLimitsPatch) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		if _, err := q.GetOrgSlug(ctx, org); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		row, err := q.GetOrgSettings(ctx, org)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		before := runLimitsOf(apply(Defaults(), row))
		next := store.SetOrgRunLimitsParams{
			OrgID:                org,
			MaxConcurrentRuns:    merge(row.MaxConcurrentRuns, p.MaxConcurrentRuns),
			TriggerConcurrency:   merge(row.TriggerConcurrency, p.TriggerConcurrency),
			TriggerRatePerMinute: merge(row.TriggerRatePerMinute, p.TriggerRatePerMinute),
		}
		if err := q.SetOrgRunLimits(ctx, next); err != nil {
			return err
		}
		after := before
		if next.MaxConcurrentRuns.Valid {
			after.MaxConcurrentRuns = int(next.MaxConcurrentRuns.Int32)
		} else {
			after.MaxConcurrentRuns = DefaultMaxConcurrentRuns
		}
		if next.TriggerConcurrency.Valid {
			after.TriggerConcurrency = int(next.TriggerConcurrency.Int32)
		} else {
			after.TriggerConcurrency = DefaultTriggerConcurrency
		}
		if next.TriggerRatePerMinute.Valid {
			after.TriggerRatePerMinute = int(next.TriggerRatePerMinute.Int32)
		} else {
			after.TriggerRatePerMinute = DefaultTriggerRatePerMinute
		}
		if before == after {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: runLimitsSetAction, TargetKind: "org", TargetID: org.String(),
			Before: limitsSummary(before), After: limitsSummary(after),
		})
		return nil
	})
}

func limitsSummary(l RunLimits) map[string]any {
	return map[string]any{
		"max_concurrent_runs": l.MaxConcurrentRuns, "trigger_concurrency": l.TriggerConcurrency,
		"trigger_rate_per_minute": l.TriggerRatePerMinute,
	}
}
