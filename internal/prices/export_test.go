// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/pricing"
)

// SetPlatformForTest writes a platform version holding exactly the given entries, valid from now
// or, when validFrom is set, from then (a past time is refused like in production).
func (s *Service) SetPlatformForTest(ctx context.Context, entries []pricing.Entry, validFrom ...time.Time) (PlatformVersion, error) {
	var out PlatformVersion
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		w := versionWrite{Actor: audit.CLI("test"), Action: "platform_price.set", Reason: "test"}
		if len(validFrom) > 0 {
			w.ValidFrom = validFrom[0]
		}
		for _, e := range entries {
			w.Entries = append(w.Entries, PlatformEntry{Entry: e, Reviewed: e.Spec})
			w.Changed = append(w.Changed, e.ID)
		}
		var err error
		out, err = writeVersion(ctx, q, b, w)
		return err
	})
	return out, err
}

// TickForTest is one round of the schedule in the configured setting, as if Effective allowed it.
func (r *Refresher) TickForTest(ctx context.Context, mode RefreshMode, interval time.Duration) (RunResult, error) {
	return r.runSchedule(ctx, r.Configured(mode, interval))
}

// RunScheduleForTest is one round of the schedule sc (Loop runs it with sc from Effective).
func (r *Refresher) RunScheduleForTest(ctx context.Context, sc Schedule) (RunResult, error) {
	return r.runSchedule(ctx, sc)
}
