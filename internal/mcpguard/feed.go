// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
)

// ErrNoGeneration is Feed.Report's error for an agent whose sandbox has no home generation yet:
// there is no session to record the report in.
var ErrNoGeneration = errors.New("mcpguard: the agent's sandbox has no home generation")

// Feed is agentd's side of the taint store (ADR 0084 decision 2, ADR 0091 decision 4): the results
// of the sandbox's own tool calls that agentd sees (repository reads, searches, shell output, file
// edits) set the session's legs, and their text is fingerprinted with the session's key in agentd's
// process. Only the fingerprints are stored; the text never leaves agentd.
type Feed struct {
	q     *store.Queries
	state *State
}

// NewFeed returns the feed on pool. metrics may be nil.
func NewFeed(pool *pgxpool.Pool, metrics *Metrics) *Feed {
	return &Feed{q: store.New(pool), state: NewState(pool, 0, metrics)}
}

// The feed's counts (ADR 0091 decision 4, amendment 2026-11-03): agentd marks each finished tool
// call when it reads it from the harness (Read) and again once it has reported it, or found nothing
// to report (Done), under its epoch of the run. The guard waits for Done to reach Read before it
// reads the legs, so a call the harness makes right after a tool finished is decided on that tool's
// result.

// Start starts the run's counts at zero for the worker of epoch; a later Read or Done of an earlier
// epoch counts nothing.
func (f *Feed) Start(ctx context.Context, org, run pgtype.UUID, epoch int64) error {
	return f.q.StartLeakGuardFeed(ctx, store.StartLeakGuardFeedParams{OrgID: org, RunID: run, Epoch: epoch})
}

// Read counts a finished tool call agentd read from the harness and has not reported yet.
func (f *Feed) Read(ctx context.Context, org, run pgtype.UUID, epoch int64) error {
	_, err := f.q.MarkLeakGuardFeedRead(ctx, store.MarkLeakGuardFeedReadParams{OrgID: org, RunID: run, Epoch: epoch})
	return err
}

// Done counts the next finished tool call as reported.
func (f *Feed) Done(ctx context.Context, org, run pgtype.UUID, epoch int64) error {
	_, err := f.q.MarkLeakGuardFeedDone(ctx, store.MarkLeakGuardFeedDoneParams{OrgID: org, RunID: run, Epoch: epoch})
	return err
}

// Report records one tool result of an agent's sandbox: legs (they only grow) and the
// fingerprints of texts at the org's threshold.
func (f *Feed) Report(ctx context.Context, org, agent pgtype.UUID, legs Legs, texts []string) error {
	gen, ok, err := Generation(ctx, f.q, org, agent)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoGeneration
	}
	threshold, err := thresholdOf(ctx, f.q, org)
	if err != nil {
		return err
	}
	_, err = f.state.Record(ctx, Session{Org: org, Agent: agent, Generation: gen}, legs, threshold, texts)
	return err
}
