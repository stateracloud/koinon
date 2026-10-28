// SPDX-License-Identifier: Apache-2.0

// Package triggers is the read side of agent triggers (ADR 0066): which triggers are in effect.
// A trigger is in effect when it belongs to the active version of an active agent; a proposed
// version's triggers, a draft agent's and an archived agent's are not. The schedule (TR2) and
// the direct-message dispatcher (TR3) read from here and fire nothing in this package.
package triggers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/templates"
)

// Active is one trigger in effect.
type Active struct {
	OrgID   pgtype.UUID
	AgentID pgtype.UUID
	// Version is the agent version that defines the trigger.
	Version int32
	templates.Trigger
}

// ForOrg lists the triggers in effect in one org, of the given types (all types when none).
func ForOrg(ctx context.Context, q *store.Queries, org pgtype.UUID, types ...templates.TriggerType) ([]Active, error) {
	rows, err := q.ListActiveAgentVersions(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	for _, r := range rows {
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	return out, nil
}

// All lists the triggers in effect in every live org.
func All(ctx context.Context, q *store.Queries, types ...templates.TriggerType) ([]Active, error) {
	rows, err := q.ListActiveAgentVersionsAllOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	for _, r := range rows {
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	return out, nil
}

// Skipped is an active agent version whose config did not parse, so its triggers are unknown.
type Skipped struct {
	OrgID   pgtype.UUID
	AgentID pgtype.UUID
	Version int32
	Err     error
}

// AllLenient is All for a loop that must not stop at one bad row: an active version whose config
// does not parse is returned in the second result instead of failing the listing.
func AllLenient(ctx context.Context, q *store.Queries, types ...templates.TriggerType) ([]Active, []Skipped, error) {
	rows, err := q.ListActiveAgentVersionsAllOrgs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	var skipped []Skipped
	for _, r := range rows {
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			skipped = append(skipped, Skipped{OrgID: r.OrgID, AgentID: r.AgentID, Version: r.Version, Err: err})
			continue
		}
		out = append(out, a...)
	}
	return out, skipped, nil
}

// ForAgent lists the triggers in effect on one agent: empty when it is not active.
func ForAgent(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, types ...templates.TriggerType) ([]Active, error) {
	rows, err := q.ListActiveAgentVersions(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	for _, r := range rows {
		// Only this agent's config is parsed: another agent's bad row is not this caller's error.
		if r.AgentID != agent {
			continue
		}
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	return out, nil
}

func collect(org, agent pgtype.UUID, version int32, raw []byte, types []templates.TriggerType) ([]Active, error) {
	cfg, err := agentconfig.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("agent %s version %d: %w", agent, version, err)
	}
	var out []Active
	for _, tr := range cfg.Triggers {
		if len(types) > 0 && !contains(types, tr.Type) {
			continue
		}
		out = append(out, Active{OrgID: org, AgentID: agent, Version: version, Trigger: tr})
	}
	return out, nil
}

func contains(ts []templates.TriggerType, t templates.TriggerType) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// UUID names a trigger in a task's creator field and in dedupe keys: a trigger ID is a slug that is
// unique only within its agent, so the UUID is derived from both.
func UUID(agent pgtype.UUID, trigger string) pgtype.UUID {
	sum := sha256.Sum256(append(append([]byte("koinon-trigger/"), agent.Bytes[:]...), trigger...))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x50
	id[8] = id[8]&0x3f | 0x80
	return pgtype.UUID{Bytes: id, Valid: true}
}

// Fire kinds: how a trigger came to fire, recorded in the task's events so reports can tell a
// scheduled fire from an admin's "run now".
const (
	FireScheduled = "scheduled"
	FireManual    = "manual"
)

// ScheduleTaskTitle is the title of the task a schedule trigger creates: the trigger's name and the
// due time in the trigger's time zone.
func ScheduleTaskTitle(trigger string, due time.Time, loc *time.Location) string {
	return fmt.Sprintf("Scheduled: %s, %s", trigger, due.In(loc).Format("2006-01-02 15:04 MST"))
}

// ManualTaskTitle is the title of the task an admin's run-now creates.
func ManualTaskTitle(trigger string, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("Run now: %s, %s", trigger, at.In(loc).Format("2006-01-02 15:04 MST"))
}

// ErrTriggerNotFound means the agent's active version has no schedule trigger of that ID.
var ErrTriggerNotFound = errors.New("the agent has no schedule trigger of that ID in its active version")

// ErrCannotRun means the agent cannot take a run now (archived, a draft, paused, or unhealthy). The
// error wraps the reason.
var ErrCannotRun = errors.New("the agent cannot take a run now")

// RunNow is a request to fire one schedule trigger immediately.
type RunNow struct {
	Org, Agent pgtype.UUID
	// Trigger is the trigger's ID in the agent's active version.
	Trigger string
	// Actor is who asked, for the audit event; User is that person's user ID when it is a user.
	Actor audit.Actor
	User  pgtype.UUID
	// Now is the time of the fire; zero is the current time.
	Now time.Time
}
