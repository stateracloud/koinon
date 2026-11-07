// SPDX-License-Identifier: Apache-2.0

// Package orgsettings resolves the per-org latency tunables of plan item LT1 (ADRs 0049, 0065,
// 0066): how long an idle sandbox stays up, how long one whose run waits on a person or another
// agent stays warm, and how long a run waits for its sandbox before it goes back to the queue, and the most it waits for
// it in total, across requeues, before it fails (old issue 160).
// A value the org did not set is the platform default, which the caller passes in (the
// reconciler's and agentd's own configuration).
package orgsettings

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

// Platform defaults (ADR 0066 decision 11).
const (
	DefaultIdleTimeout = 10 * time.Minute
	DefaultParkWarm    = 15 * time.Minute
	DefaultQueueWait   = 5 * time.Minute
	// DefaultSandboxWaitCap is the most a run waits for its sandbox in total, across requeues.
	DefaultSandboxWaitCap = time.Hour

	// DefaultMaxConcurrentRuns is the cap on an org's owned runs. The same number is in
	// run_claimable (migration 00023); the two must change together.
	DefaultMaxConcurrentRuns = 20
	// DefaultTriggerConcurrency and DefaultTriggerRatePerMinute are an org's share of the DBOS queue
	// for trigger, webhook and task-creation work (DB2, ADR 0066).
	DefaultTriggerConcurrency   = 4
	DefaultTriggerRatePerMinute = 60
)

// Settings are the effective tunables of one org.
type Settings struct {
	// IdleTimeout stops a sandbox after this long without activity.
	IdleTimeout time.Duration
	// ParkWarm keeps a sandbox up this long after its run parked on a2a or input_required.
	ParkWarm time.Duration
	// QueueWait is how long a claimed run waits for its sandbox before it returns to the queue.
	QueueWait time.Duration
	// SandboxWaitCap is the most a run waits for its sandbox in total, across requeues; then it fails
	// (sandbox_unavailable).
	SandboxWaitCap time.Duration
	// MaxConcurrentRuns caps the runs the org has starting or running at once (run_claimable).
	MaxConcurrentRuns int
	// TriggerConcurrency is how many trigger, webhook and task-creation items of the org run at
	// once; TriggerRatePerMinute is how many may start per minute (internal/durable).
	TriggerConcurrency, TriggerRatePerMinute int
	// RunContentRetention is how long a finished run's stored content is kept (retention.go).
	RunContentRetention time.Duration
	// HomeBackupRetention is how long an old home backup repository is kept after a key change
	// (homebackup.go).
	HomeBackupRetention time.Duration
}

// Defaults are the platform defaults a Settings falls back to.
func Defaults() Settings {
	return Settings{
		IdleTimeout: DefaultIdleTimeout, ParkWarm: DefaultParkWarm, QueueWait: DefaultQueueWait,
		SandboxWaitCap: DefaultSandboxWaitCap, RunContentRetention: DefaultRunContentRetention,
		MaxConcurrentRuns: DefaultMaxConcurrentRuns, TriggerConcurrency: DefaultTriggerConcurrency,
		TriggerRatePerMinute: DefaultTriggerRatePerMinute, HomeBackupRetention: DefaultHomeBackupRetention,
	}
}

// Get returns the org's settings, with def filling every value the org did not set.
func Get(ctx context.Context, q *store.Queries, org pgtype.UUID, def Settings) (Settings, error) {
	row, err := q.GetOrgSettings(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	return apply(def, row), nil
}

func apply(def Settings, row store.OrgSetting) Settings {
	if row.IdleTimeoutSeconds.Valid {
		def.IdleTimeout = time.Duration(row.IdleTimeoutSeconds.Int32) * time.Second
	}
	if row.ParkWarmSeconds.Valid {
		def.ParkWarm = time.Duration(row.ParkWarmSeconds.Int32) * time.Second
	}
	if row.QueueWaitSeconds.Valid {
		def.QueueWait = time.Duration(row.QueueWaitSeconds.Int32) * time.Second
	}
	if row.SandboxWaitCapSeconds.Valid {
		def.SandboxWaitCap = time.Duration(row.SandboxWaitCapSeconds.Int32) * time.Second
	}
	if row.MaxConcurrentRuns.Valid {
		def.MaxConcurrentRuns = int(row.MaxConcurrentRuns.Int32)
	}
	if row.TriggerConcurrency.Valid {
		def.TriggerConcurrency = int(row.TriggerConcurrency.Int32)
	}
	if row.TriggerRatePerMinute.Valid {
		def.TriggerRatePerMinute = int(row.TriggerRatePerMinute.Int32)
	}
	if row.RunContentRetentionDays.Valid {
		def.RunContentRetention = time.Duration(row.RunContentRetentionDays.Int32) * 24 * time.Hour
	}
	if row.HomeBackupRetentionDays.Valid {
		def.HomeBackupRetention = time.Duration(row.HomeBackupRetentionDays.Int32) * 24 * time.Hour
	}
	return def
}

// Override is a change to the org's settings; a nil field means the platform default.
type Override struct {
	IdleTimeout, ParkWarm, QueueWait *time.Duration
	// SandboxWaitCap is the total wait for a sandbox (300 s to 7 days).
	SandboxWaitCap *time.Duration
	// MaxConcurrentRuns, TriggerConcurrency and TriggerRatePerMinute are the DB2 limits.
	MaxConcurrentRuns, TriggerConcurrency, TriggerRatePerMinute *int
}

// Set replaces the org's overrides. The database bounds each value (see migrations 00022 and 00023).
func Set(ctx context.Context, q *store.Queries, org pgtype.UUID, o Override) error {
	_, err := q.UpsertOrgSettings(ctx, store.UpsertOrgSettingsParams{
		OrgID:                 org,
		IdleTimeoutSeconds:    seconds(o.IdleTimeout),
		ParkWarmSeconds:       seconds(o.ParkWarm),
		QueueWaitSeconds:      seconds(o.QueueWait),
		SandboxWaitCapSeconds: seconds(o.SandboxWaitCap),

		MaxConcurrentRuns:    count(o.MaxConcurrentRuns),
		TriggerConcurrency:   count(o.TriggerConcurrency),
		TriggerRatePerMinute: count(o.TriggerRatePerMinute),
	})
	return err
}

func seconds(d *time.Duration) pgtype.Int4 {
	if d == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*d / time.Second), Valid: true} //nolint:gosec // bounded by the table's CHECK
}

func count(n *int) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*n), Valid: true} //nolint:gosec // bounded by the table's CHECK
}

// QueueDefaults are the platform defaults as limits for durable.OrgQueueConfig.Defaults, so an org
// whose settings are left at the default stays on the main queue.
func QueueDefaults() durable.OrgLimits {
	return durable.OrgLimits{Concurrency: DefaultTriggerConcurrency, RatePerWindow: DefaultTriggerRatePerMinute}
}

// QueueLimits connects an org's trigger_concurrency and trigger_rate_per_minute to the
// org-partitioned queue. The rate is per minute, so it assumes OrgQueueConfig.RateWindow is the
// default one minute. org is the org's UUID string; def is the settings default (Defaults()).
func QueueLimits(q *store.Queries, def Settings) durable.LimitsFunc {
	return func(ctx context.Context, org string) (durable.OrgLimits, error) {
		var id pgtype.UUID
		if err := id.Scan(org); err != nil {
			return durable.OrgLimits{}, err
		}
		s, err := Get(ctx, q, id, def)
		if err != nil {
			return durable.OrgLimits{}, err
		}
		return durable.OrgLimits{Concurrency: s.TriggerConcurrency, RatePerWindow: s.TriggerRatePerMinute}, nil
	}
}
