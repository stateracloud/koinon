// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Retention of old home backup repositories (issue #219, part of #85). A key change starts a new
// home repository epoch (ADR 0085 decision 13); the old one stays readable for the checkpoints
// written to it for the org's retention and is then deleted by agentd's sweeper. The value is read
// when the new epoch starts (checkpoint.Passwords.NewEpoch): a later change does not move the
// deadline of a repository that is already retired.

const (
	// DefaultHomeBackupRetentionDays is the retention of an org that set none. It equals
	// checkpoint.DefaultRetain (a test checks it; orgsettings does not import the kopia writer).
	DefaultHomeBackupRetentionDays = 7
	// DefaultHomeBackupRetention is DefaultHomeBackupRetentionDays as a duration.
	DefaultHomeBackupRetention = DefaultHomeBackupRetentionDays * 24 * time.Hour
	// MinHomeBackupRetentionDays and MaxHomeBackupRetentionDays bound the setting (the column's CHECK).
	MinHomeBackupRetentionDays = 1
	MaxHomeBackupRetentionDays = 90
)

// ValidateHomeBackupRetentionDays checks a retention in days.
func ValidateHomeBackupRetentionDays(n int) error {
	if n < MinHomeBackupRetentionDays || n > MaxHomeBackupRetentionDays {
		return &InvalidError{Msg: fmt.Sprintf("home backup retention must be %d to %d days", MinHomeBackupRetentionDays, MaxHomeBackupRetentionDays)}
	}
	return nil
}

// HomeBackupRetention returns how long the org keeps an old home backup repository: the retain to
// pass to checkpoint.Passwords.NewEpoch.
func (s *Service) HomeBackupRetention(ctx context.Context, org pgtype.UUID) (time.Duration, error) {
	st, err := Get(ctx, store.New(s.pool), org, Defaults())
	if err != nil {
		return 0, err
	}
	return st.HomeBackupRetention, nil
}

// HomeBackupRetentionDays returns the org's effective retention in days.
func (s *Service) HomeBackupRetentionDays(ctx context.Context, org pgtype.UUID) (int, error) {
	d, err := s.HomeBackupRetention(ctx, org)
	if err != nil {
		return 0, err
	}
	return int(d / (24 * time.Hour)), nil
}

// SetHomeBackupRetention sets the org's retention in days; nil returns it to the platform default
// (7 days). A change is audited as org.home_backup_retention.set.
func (s *Service) SetHomeBackupRetention(ctx context.Context, org pgtype.UUID, actor audit.Actor, days *int) error {
	set := pgtype.Int4{}
	if days != nil {
		if err := ValidateHomeBackupRetentionDays(*days); err != nil {
			return err
		}
		set = pgtype.Int4{Int32: int32(*days), Valid: true} //nolint:gosec // bounded by ValidateHomeBackupRetentionDays
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := Get(ctx, q, org, Defaults())
		if err != nil {
			return err
		}
		before := int(cur.HomeBackupRetention / (24 * time.Hour))
		if err := q.SetOrgHomeBackupRetention(ctx, store.SetOrgHomeBackupRetentionParams{OrgID: org, HomeBackupRetentionDays: set}); err != nil {
			return err
		}
		after := DefaultHomeBackupRetentionDays
		if days != nil {
			after = *days
		}
		if before == after {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.home_backup_retention.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"days": before}, After: map[string]any{"days": after},
		})
		return nil
	})
}
