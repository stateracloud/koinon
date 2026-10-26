// SPDX-License-Identifier: Apache-2.0

// Package orgsettings resolves the per-org latency tunables of plan item LT1 (ADRs 0049, 0065,
// 0066): how long an idle sandbox stays up, how long one whose run waits on a person or another
// agent stays warm, and how long a run waits for its sandbox before it goes back to the queue.
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
)

// Platform defaults (ADR 0066 decision 11).
const (
	DefaultIdleTimeout = 10 * time.Minute
	DefaultParkWarm    = 15 * time.Minute
	DefaultQueueWait   = 5 * time.Minute
)

// Settings are the effective tunables of one org.
type Settings struct {
	// IdleTimeout stops a sandbox after this long without activity.
	IdleTimeout time.Duration
	// ParkWarm keeps a sandbox up this long after its run parked on a2a or input_required.
	ParkWarm time.Duration
	// QueueWait is how long a claimed run waits for its sandbox before it returns to the queue.
	QueueWait time.Duration
}

// Defaults are the platform defaults a Settings falls back to.
func Defaults() Settings {
	return Settings{IdleTimeout: DefaultIdleTimeout, ParkWarm: DefaultParkWarm, QueueWait: DefaultQueueWait}
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
	return def
}

// Override is a change to the org's settings; a nil field means the platform default.
type Override struct {
	IdleTimeout, ParkWarm, QueueWait *time.Duration
}

// Set replaces the org's overrides. The database bounds each value (see migration 00022).
func Set(ctx context.Context, q *store.Queries, org pgtype.UUID, o Override) error {
	_, err := q.UpsertOrgSettings(ctx, store.UpsertOrgSettingsParams{
		OrgID:              org,
		IdleTimeoutSeconds: seconds(o.IdleTimeout),
		ParkWarmSeconds:    seconds(o.ParkWarm),
		QueueWaitSeconds:   seconds(o.QueueWait),
	})
	return err
}

func seconds(d *time.Duration) pgtype.Int4 {
	if d == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*d / time.Second), Valid: true} //nolint:gosec // bounded by the table's CHECK
}
