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
