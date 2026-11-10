// SPDX-License-Identifier: Apache-2.0

package tracedest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/tracedest"
)

// A held install sends no synthetic trace: every method that would returns ErrHeld before it reads
// the database or dials (issue #462, ADR 0107). The nil client would panic on a dial.
func TestSendsRefuseWhileHeld(t *testing.T) {
	svc := tracedest.NewWithClient(nil, nil, catalog.URLPolicy{}, nil, nil).WithRestoreHold(restorehold.NewGuard(true))
	ctx, org, actor := context.Background(), pgtype.UUID{}, audit.Actor{}
	calls := map[string]func() error{
		"Create": func() error {
			_, err := svc.Create(ctx, org, actor, pgtype.UUID{}, tracedest.Authority{}, tracedest.Input{})
			return err
		},
		"Update": func() error {
			_, err := svc.Update(ctx, org, actor, tracedest.Authority{}, "x", nil, tracedest.Input{})
			return err
		},
		"Test":                   func() error { _, _, err := svc.Test(ctx, org, actor, "x"); return err },
		"SetPlatformDestination": func() error { _, err := svc.SetPlatformDestination(ctx, actor, nil, tracedest.Input{}); return err },
		"TestPlatform":           func() error { _, _, err := svc.TestPlatform(ctx, actor); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, restorehold.ErrHeld) {
			t.Errorf("%s: err = %v, want ErrHeld", name, err)
		}
	}
}
