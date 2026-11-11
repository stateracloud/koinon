// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/templates"
)

// Due is one schedule trigger that the first tick after a release fires once (the catch-up).
type Due struct {
	Org     string    `json:"org"`
	Agent   string    `json:"agent"`
	Trigger string    `json:"trigger"`
	DueAt   time.Time `json:"dueAt"`
}

// PreviewCatchUp lists the fires the first tick of a scheduler started at now enqueues: for each
// schedule trigger in effect, the latest due time in the look-back window (DefaultCatchUp) whose start
// has passed, never before the version that defines the trigger was activated. It runs the same
// planning as Tick and writes nothing, wakes nothing and starts no workflow, so it is safe on a held
// install (ADR 0110 decision 7).
func PreviewCatchUp(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]Due, error) {
	s := New(Config{Now: func() time.Time { return now }}, pool, nil)
	from := now.Add(-s.cfg.CatchUp)
	list, _, err := triggers.AllLenient(ctx, s.q, templates.TriggerSchedule)
	if err != nil {
		return nil, err
	}
	acts, err := s.q.ListAgentActivationsAllOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list activations: %w", err)
	}
	activated := map[pgtype.UUID]time.Time{}
	for _, a := range acts {
		activated[a.AgentID] = a.ActivatedAt.Time
	}
	var out []Due
	for _, tr := range list {
		sched, loc, err := templates.ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
		if err != nil {
			continue // the scheduler skips it too
		}
		fire, _ := s.plan(tr, sched, loc, activated[tr.AgentID], from, now)
		if fire.IsZero() {
			continue
		}
		out = append(out, Due{Org: tr.OrgID.String(), Agent: tr.AgentID.String(), Trigger: tr.ID, DueAt: fire})
	}
	return out, nil
}
