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
	MinMaxConcurrentRuns, MaxMaxConcurrentRuns                 = 1, 10000
	MinTriggerConcurrency, MaxTriggerConcurrency               = 1, 1000
	MinTriggerRatePerMinute, MaxTriggerRatePerMinute           = 1, 100000
	MinSandboxCreationsPerMinute, MaxSandboxCreationsPerMinute = 1, 10000
	runLimitsSetAction                                         = "org.run_limits.set"
)

// RunLimits are the org's effective run limits.
type RunLimits struct {
	MaxConcurrentRuns, TriggerConcurrency, TriggerRatePerMinute int
	// SandboxCreationsPerMinute is the most sandbox creations the reconciler makes for the org per minute.
	SandboxCreationsPerMinute int
}

// RunLimitsPatch changes some of the org's run limits. A nil field stays as it is; a field that
// points at 0 returns to the platform default; any other value is the new limit.
type RunLimitsPatch struct {
	MaxConcurrentRuns, TriggerConcurrency, TriggerRatePerMinute, SandboxCreationsPerMinute *int
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
		checkLimit("sandbox creations per minute", p.SandboxCreationsPerMinute, MinSandboxCreationsPerMinute, MaxSandboxCreationsPerMinute),
	)
}

func runLimitsOf(s Settings) RunLimits {
	return RunLimits{MaxConcurrentRuns: s.MaxConcurrentRuns, TriggerConcurrency: s.TriggerConcurrency, TriggerRatePerMinute: s.TriggerRatePerMinute,
		SandboxCreationsPerMinute: s.SandboxCreationsPerMinute}
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
// with the stored limits before and after (null is the default). Other settings of the org stay. An org that does not
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
		// Two admins setting at once must each audit the other's result as their before: the row lock
		// (the row made first, as the fetch-trusted list does) holds the second until the first commits.
		if err := q.EnsureOrgSettings(ctx, org); err != nil {
			return err
		}
		if _, err := q.LockOrgSettings(ctx, org); err != nil {
			return err
		}
		row, err := q.GetOrgSettings(ctx, org)
		if err != nil {
			return err
		}
		next := store.SetOrgRunLimitsParams{
			OrgID:                org,
			MaxConcurrentRuns:    merge(row.MaxConcurrentRuns, p.MaxConcurrentRuns),
			TriggerConcurrency:   merge(row.TriggerConcurrency, p.TriggerConcurrency),
			TriggerRatePerMinute: merge(row.TriggerRatePerMinute, p.TriggerRatePerMinute),
		}
		if err := q.SetOrgRunLimits(ctx, next); err != nil {
			return err
		}
		// What changed is the stored row, not the effective limits: NULL (the default) to an explicit
		// value equal to the default is a change, and the audit trail shows it.
		if row.MaxConcurrentRuns == next.MaxConcurrentRuns && row.TriggerConcurrency == next.TriggerConcurrency &&
			row.TriggerRatePerMinute == next.TriggerRatePerMinute &&
			row.SandboxCreationsPerMinute == next.SandboxCreationsPerMinute {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: runLimitsSetAction, TargetKind: "org", TargetID: org.String(),
			Before: limitsSummary(row.MaxConcurrentRuns, row.TriggerConcurrency, row.TriggerRatePerMinute, row.SandboxCreationsPerMinute),
			After:  limitsSummary(next.MaxConcurrentRuns, next.TriggerConcurrency, next.TriggerRatePerMinute, next.SandboxCreationsPerMinute),
		})
		return nil
	})
}

// limitsSummary is the audit summary of the stored limits: null is "the default", so NULL to an
// explicit value equal to the default shows as a change.
func limitsSummary(maxRuns, triggerConcurrency, ratePerMinute, creations pgtype.Int4) map[string]any {
	stored := func(v pgtype.Int4) any {
		if !v.Valid {
			return nil
		}
		return int(v.Int32)
	}
	return map[string]any{
		"max_concurrent_runs": stored(maxRuns), "trigger_concurrency": stored(triggerConcurrency),
		"trigger_rate_per_minute": stored(ratePerMinute),
	}
}
