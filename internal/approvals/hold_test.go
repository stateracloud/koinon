// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// A held install executes no approved call and approves none (Deny stays open): Approve, Execute
// and Diff return ErrHeld before they touch the database or any code host (issue #462, ADR 0107).
func TestApprovalsRefuseWhileHeld(t *testing.T) {
	svc := approvals.New(nil, approvals.Options{RestoreHold: restorehold.NewGuard(true)})
	ctx, id := context.Background(), pgtype.UUID{}
	if _, err := svc.Approve(ctx, id, id, id, "sha", ""); !errors.Is(err, restorehold.ErrHeld) {
		t.Errorf("Approve: err = %v, want ErrHeld", err)
	}
	if _, err := svc.ApproveScoped(ctx, id, id, id, "sha", "", approvals.Scope{}); !errors.Is(err, restorehold.ErrHeld) {
		t.Errorf("ApproveScoped: err = %v, want ErrHeld", err)
	}
	if _, err := svc.Execute(ctx, id, id); !errors.Is(err, restorehold.ErrHeld) {
		t.Errorf("Execute: err = %v, want ErrHeld", err)
	}
	if _, err := svc.Diff(ctx, store.ApprovalRequest{}); !errors.Is(err, restorehold.ErrHeld) {
		t.Errorf("Diff: err = %v, want ErrHeld", err)
	}
}
