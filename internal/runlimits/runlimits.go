// SPDX-License-Identifier: Apache-2.0

// Package runlimits is what bounds a single run (ADR 0080): the model-request limit, the spend
// limit and the wall clock. The first one reached ends the run. Tokens are reported, not enforced.
//
// A run copies its limits when it is queued: from its agent version (the template's values), over
// which an admin's per-agent override ([Override]) applies. What a run has used is read from its
// ledger entries ([ReadUsage]), so the count of requests and the spend are exact and need no
// counter that can drift when an entry is settled later.
//
// [Nearest] says which limit is closest to being reached; the wrap-up notice (ADR 0066 amendment
// 2026-10-28, ADR 0080) is sent when it reaches [WrapUpFraction].
package runlimits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
)

// Kind names a run limit. The values are the fixed label set of the metrics that count stops by
// limit (ADR 0023) and the "limit" of a wrap-up notice event.
type Kind string

// The run limits.
const (
	KindTime     Kind = "time"
	KindRequests Kind = "requests"
	KindSpend    Kind = "spend"
)

// Kinds lists every limit, for metrics that pre-create their label values.
var Kinds = []Kind{KindTime, KindRequests, KindSpend}

// WrapUpFraction is how far through a limit the agent is told to wrap up: about 80%.
const WrapUpFraction = 0.8

// Limits are the limits of one run. A zero value is no limit, which only a run queued before ADR
// 0080 has for requests and spend.
type Limits struct {
	// WallClock is the time the run may be owned; parked time does not count.
	WallClock time.Duration
	// ParkedTime is the time the run has spent parked, added to the wall clock.
	ParkedTime       time.Duration
	MaxModelRequests int64
	MaxSpend         money.USD
}

// Of returns the limits a stored run carries.
func Of(r store.Run) (Limits, error) {
	l := Limits{
		WallClock:        time.Duration(r.MaxWallClockSeconds) * time.Second,
		ParkedTime:       time.Duration(r.ParkedSeconds) * time.Second,
		MaxModelRequests: r.MaxModelRequests.Int64,
	}
	spend, _, err := spendOf(r)
	l.MaxSpend = spend
	return l, err
}

func spendOf(r store.Run) (money.USD, bool, error) {
	if !r.MaxSpendUsd.Valid {
		return 0, false, nil
	}
	v, err := r.MaxSpendUsd.Value()
	if err != nil {
		return 0, false, fmt.Errorf("runlimits: spend limit: %w", err)
	}
	s, _ := v.(string)
	u, err := money.ParseNumeric(s)
	return u, err == nil && u > 0, err
}

// Usage is what a run has used so far, from its ledger entries.
type Usage struct {
	// Requests is one per entry: every model request through the agent's key, the harness's own
	// title, summary and compaction calls included.
	Requests int64
	// Spend is the entries' settled cost (estimates and provisional charges included until they
	// settle). Reserve is what provisional entries hold back from the gateway key; it is not spend.
	Spend, Reserve money.USD
	// Tokens by type. Input includes the cache reads and writes (the gateway's convention), so the
	// uncached input is [Usage.UncachedInput]. Reasoning is part of Output where reported.
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	// Estimated counts the requests whose usage Koinon estimated.
	Estimated int64
}

// UncachedInput is the input that was neither read from nor written to the provider's cache.
func (u Usage) UncachedInput() int64 { return max(u.Input-u.CacheRead-u.CacheWrite, 0) }

// Tokens is input plus output, the figure reported as the run's tokens used.
func (u Usage) Tokens() int64 { return u.Input + u.Output }

// ReadUsage sums the run's ledger entries.
func ReadUsage(ctx context.Context, q *store.Queries, org, run pgtype.UUID) (Usage, error) {
	row, err := q.GetRunUsage(ctx, store.GetRunUsageParams{OrgID: org, RunID: run})
	if err != nil {
		return Usage{}, err
	}
	spend, err := money.ParseNumeric(row.CostUsd)
	if err != nil {
		return Usage{}, err
	}
	reserve, err := money.ParseNumeric(row.ReserveUsd)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		Requests: row.Requests, Spend: spend, Reserve: reserve,
		Input: row.InputTokens, Output: row.OutputTokens, CacheRead: row.CacheReadTokens,
		CacheWrite: row.CacheWriteTokens, Reasoning: row.ReasoningTokens, Estimated: row.EstimatedRequests,
	}, nil
}

// RequestsReached is the model-request limit of a run that has made that many requests. The
// spend limit is a spend scope ([budget.Level] of kind RunSpend), checked with the other spend
// limits.
type RequestsReached struct{ Used, Max int64 }

// Message is why the run ended, as its outcome reason.
func (r RequestsReached) Message() string {
	return fmt.Sprintf("model request limit reached (%d of %d requests)", r.Used, r.Max)
}

// Detail is the limit's part of the payload of the run's cancel event.
func (r RequestsReached) Detail() map[string]any {
	return map[string]any{"limit": string(KindRequests), "max_model_requests": r.Max, "model_requests": r.Used}
}

// CheckRequests reports whether u has reached the request limit. A run is canceled when it has
// made the limit's number of requests: the Nth request completed, and the next would be the N+1th.
func (l Limits) CheckRequests(u Usage) (RequestsReached, bool) {
	if l.MaxModelRequests > 0 && u.Requests >= l.MaxModelRequests {
		return RequestsReached{Used: u.Requests, Max: l.MaxModelRequests}, true
	}
	return RequestsReached{}, false
}

// Progress is how far a run is through one limit.
type Progress struct {
	Kind Kind
	// Fraction is used/max, above 1 when the limit is passed.
	Fraction float64
}

// Nearest returns the limit the run is closest to reaching, by fraction used. elapsed is the time
// the run has been owned, parked time excluded. ok is false when no limit is set.
func (l Limits) Nearest(u Usage, elapsed time.Duration) (Progress, bool) {
	var best Progress
	found := false
	consider := func(k Kind, f float64) {
		if !found || f > best.Fraction {
			best, found = Progress{Kind: k, Fraction: f}, true
		}
	}
	if l.WallClock > 0 {
		consider(KindTime, float64(elapsed)/float64(l.WallClock))
	}
	if l.MaxModelRequests > 0 {
		consider(KindRequests, float64(u.Requests)/float64(l.MaxModelRequests))
	}
	if l.MaxSpend > 0 {
		consider(KindSpend, money.Ratio(u.Spend, l.MaxSpend))
	}
	return best, found
}

// Override is an admin's per-agent override of the template's limits. A zero field follows the
// agent version.
type Override struct {
	MaxModelRequests int64
	MaxSpend         money.USD
	WallClock        time.Duration
}

// IsZero reports whether the override sets nothing.
func (o Override) IsZero() bool { return o == Override{} }

// GetOverride reads the agent's override; the zero Override when it has none.
func GetOverride(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (Override, error) {
	row, err := q.GetAgentRunLimits(ctx, store.GetAgentRunLimitsParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return Override{}, nil
	} else if err != nil {
		return Override{}, err
	}
	o := Override{MaxModelRequests: row.MaxModelRequests.Int64, WallClock: time.Duration(row.MaxDurationSecs.Int32) * time.Second}
	if row.MaxSpendUsd != "" {
		if o.MaxSpend, err = money.ParseNumeric(row.MaxSpendUsd); err != nil {
			return Override{}, err
		}
	}
	return o, nil
}

// Apply returns base with the override's set fields.
func (o Override) Apply(base agentconfig.RunLimits) agentconfig.RunLimits {
	if o.MaxModelRequests > 0 {
		base.MaxModelRequests = o.MaxModelRequests
	}
	if o.MaxSpend > 0 {
		base.MaxSpend = o.MaxSpend
	}
	if o.WallClock > 0 {
		base.WallClock = o.WallClock
	}
	return base
}

// ForAgent returns the limits a run of the agent gets now: the version's, then the admin's
// override over them.
func ForAgent(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, cfg agentconfig.Config) (agentconfig.RunLimits, error) {
	o, err := GetOverride(ctx, q, org, agent)
	if err != nil {
		return agentconfig.RunLimits{}, fmt.Errorf("read the agent's run limits: %w", err)
	}
	return o.Apply(cfg.RunLimits()), nil
}
