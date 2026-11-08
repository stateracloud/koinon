// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Retention of stored run content (issue #69, #50). The content of a finished run's events
// (run_events.payload) is kept for the org's retention period and then blanked by the purge job. The
// event rows stay (run, seq, kind, created_at), so the run page keeps its skeleton and seq stays
// gap-free. Traces and the spend ledger are other tables and are never touched.

const (
	// DefaultRunContentRetentionDays is the retention of an org that set none.
	DefaultRunContentRetentionDays = 30
	// DefaultRunContentRetention is DefaultRunContentRetentionDays as a duration.
	DefaultRunContentRetention = DefaultRunContentRetentionDays * 24 * time.Hour
	// MinRunContentRetentionDays and MaxRunContentRetentionDays bound the setting (the column's CHECK).
	MinRunContentRetentionDays = 1
	MaxRunContentRetentionDays = 365
	// PurgeBatchSize is how many events one purge statement blanks, so no statement holds many row
	// locks for long.
	PurgeBatchSize = 1000
)

// ContentEventKinds are the run event kinds whose payload is content: what the agent and the user
// said, its thoughts and its tool calls (ADR 0074, amendment of 2026-10-31, item 5). Only these are
// blanked. Every other kind (state changes, park and resume, approvals, cost and retry notes) keeps
// its payload, so the run's state and cost stay readable after the content is gone.
var ContentEventKinds = []string{
	"acp.agent_message_chunk",
	"acp.agent_thought_chunk",
	"acp.user_message_chunk",
	"acp.tool_call",
	"acp.tool_call_update",
	"acp.plan",
}

// StrippedContentKeys are event kinds that mix content with state: the purge removes only the listed
// payload keys and keeps the rest (the prompt text but not throughSeq, the tool call of a permission
// answer but not its decision). Keys are the payload's own, as agentd records them.
var StrippedContentKeys = map[string][]string{
	"run.prompt":     {"text"},
	"acp.permission": {"toolCall"},
	"run.refusal":    {"refusals"},
}

// purgedKinds is every kind a purge touches: the blanked ones and the stripped ones.
func purgedKinds() []string {
	kinds := slices.Clone(ContentEventKinds)
	for k := range StrippedContentKeys {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}

// ValidateRunContentRetentionDays checks a retention in days.
func ValidateRunContentRetentionDays(n int) error {
	if n < MinRunContentRetentionDays || n > MaxRunContentRetentionDays {
		return &InvalidError{Msg: fmt.Sprintf("run content retention must be %d to %d days", MinRunContentRetentionDays, MaxRunContentRetentionDays)}
	}
	return nil
}

// RunContentRetentionDays returns the org's effective retention in days.
func (s *Service) RunContentRetentionDays(ctx context.Context, org pgtype.UUID) (int, error) {
	st, err := Get(ctx, store.New(s.pool), org, Defaults())
	if err != nil {
		return 0, err
	}
	return int(st.RunContentRetention / (24 * time.Hour)), nil
}

// SetRunContentRetention sets the org's retention in days; nil returns it to the platform default
// (30 days). A change is audited as org.run_content_retention.set.
func (s *Service) SetRunContentRetention(ctx context.Context, org pgtype.UUID, actor audit.Actor, days *int) error {
	set := pgtype.Int4{}
	if days != nil {
		if err := ValidateRunContentRetentionDays(*days); err != nil {
			return err
		}
		set = pgtype.Int4{Int32: int32(*days), Valid: true} //nolint:gosec // bounded by ValidateRunContentRetentionDays
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := Get(ctx, q, org, Defaults())
		if err != nil {
			return err
		}
		before := int(cur.RunContentRetention / (24 * time.Hour))
		if err := q.SetOrgRunContentRetention(ctx, store.SetOrgRunContentRetentionParams{OrgID: org, RunContentRetentionDays: set}); err != nil {
			return err
		}
		after := DefaultRunContentRetentionDays
		if days != nil {
			after = *days
		}
		if before == after {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.run_content_retention.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"days": before}, After: map[string]any{"days": after},
		})
		return nil
	})
}

// Purger blanks the stored content of runs that finished before their org's retention.
type Purger struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	now   func() time.Time
	batch int32
}

// NewPurger returns a Purger. now is the clock (time.Now in production, a fake in tests); a nil log
// discards.
func NewPurger(pool *pgxpool.Pool, log *slog.Logger, now func() time.Time) *Purger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Purger{pool: pool, log: log, now: now, batch: PurgeBatchSize}
}

// WithBatchSize sets the batch size (tests use a small one to see the batching).
func (p *Purger) WithBatchSize(n int) *Purger {
	p.batch = int32(n) //nolint:gosec // a small, caller-chosen number
	return p
}

// PurgeOrg blanks the content of events of the org's runs that finished before its retention and
// returns how many events it blanked. It is driven from runs: it walks the finished runs past the
// cutoff that are not marked purged (runs_content_unpurged), blanks each run's content events in
// batches and marks the run, so a later pass never visits it again and the cost of a pass does not
// grow with the org's recent events. Each batch is its own statement, so no lock is held for long.
// The org's own retention applies, no other org is touched.
func (p *Purger) PurgeOrg(ctx context.Context, org pgtype.UUID) (int64, error) {
	q := store.New(p.pool)
	st, err := Get(ctx, q, org, Defaults())
	if err != nil {
		return 0, err
	}
	now := p.now()
	cutoff := pgtype.Timestamptz{Time: now.Add(-st.RunContentRetention), Valid: true}
	purgedAt := pgtype.Timestamptz{Time: now, Valid: true}
	after := pgtype.Timestamptz{Time: time.Unix(0, 0).UTC(), Valid: true}
	afterID := pgtype.UUID{Valid: true}
	var total int64
	for {
		runs, err := q.ListRunsToPurgeContent(ctx, store.ListRunsToPurgeContentParams{
			OrgID: org, Cutoff: cutoff, AfterFinishedAt: after, AfterID: afterID, BatchSize: p.batch,
		})
		if err != nil {
			return total, err
		}
		for _, r := range runs {
			n, err := p.purgeRun(ctx, q, org, r.ID, purgedAt)
			total += n
			if err != nil {
				return total, err
			}
			after, afterID = r.FinishedAt, r.ID
		}
		if int32(len(runs)) < p.batch { //nolint:gosec // len of a batch bounded by p.batch
			return total, nil
		}
	}
}

// purgeRun blanks one run's content events batch by batch, then marks the run.
func (p *Purger) purgeRun(ctx context.Context, q *store.Queries, org, run pgtype.UUID, purgedAt pgtype.Timestamptz) (int64, error) {
	strip, err := json.Marshal(StrippedContentKeys)
	if err != nil {
		return 0, err
	}
	kinds := purgedKinds()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := q.PurgeRunEventContent(ctx, store.PurgeRunEventContentParams{
			OrgID: org, RunID: run, Kinds: kinds, Strip: strip, BatchSize: p.batch, PurgedAt: purgedAt,
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(p.batch) {
			break
		}
	}
	_, err = q.MarkRunContentPurged(ctx, store.MarkRunContentPurgedParams{
		OrgID: org, RunID: run, Kinds: kinds, PurgedAt: purgedAt,
	})
	return total, err
}

// PurgeAll runs PurgeOrg for every live org. One org's failure is logged and does not stop the
// others; the first error is returned after all have run.
func (p *Purger) PurgeAll(ctx context.Context) (int64, error) {
	ids, err := store.New(p.pool).ListOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	var first error
	for _, id := range ids {
		n, err := p.PurgeOrg(ctx, id)
		total += n
		if err != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			p.log.Warn("purging run content failed", "org", id.String(), "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return total, first
}

// Run purges every interval until ctx ends.
func (p *Purger) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := p.PurgeAll(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("purging run content failed", "err", err)
		} else if n > 0 {
			p.log.Info("purged stored run content", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
