// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"fmt"
	"log/slog"
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
	MaxRunContentRetentionDays = 3650
	// PurgeBatchSize is how many events one purge statement blanks, so no statement holds many row
	// locks for long.
	PurgeBatchSize = 1000
)

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
	return s.tx(ctx, func(q *store.Queries) error {
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
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "org.run_content_retention.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"days": before}, After: map[string]any{"days": after},
		})
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

// PurgeOrg blanks the content of the org's events that are past its retention and returns how many
// it blanked. Each batch is its own statement and transaction, so no lock is held for long; running
// it again finds nothing more to do. The org's own retention applies, no other org is touched.
func (p *Purger) PurgeOrg(ctx context.Context, org pgtype.UUID) (int64, error) {
	q := store.New(p.pool)
	st, err := Get(ctx, q, org, Defaults())
	if err != nil {
		return 0, err
	}
	now := p.now()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := q.PurgeRunEventContent(ctx, store.PurgeRunEventContentParams{
			OrgID:     org,
			Cutoff:    pgtype.Timestamptz{Time: now.Add(-st.RunContentRetention), Valid: true},
			BatchSize: p.batch,
			PurgedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(p.batch) {
			return total, nil
		}
	}
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
