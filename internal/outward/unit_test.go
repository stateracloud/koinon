// SPDX-License-Identifier: Apache-2.0

package outward_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"
)

func TestCheck(t *testing.T) {
	if err := outward.Check(pgtype.Timestamptz{}); err != nil {
		t.Fatalf("an enabled target: %v", err)
	}
	if err := outward.Check(pgtype.Timestamptz{Valid: true}); !errors.Is(err, outward.ErrDisabled) {
		t.Fatalf("a disabled target: %v", err)
	}
}

// Enable is refused while the install is held, before anything is read or written (ADR 0110 decision 5):
// the service has no database here, so a refusal that came from anywhere else would panic.
func TestEnableIsRefusedWhileHeld(t *testing.T) {
	svc := outward.New(nil).WithRestoreHold(restorehold.NewGuard(true))
	for _, kind := range outward.Kinds {
		_, err := svc.SetDisabled(context.Background(), pgtype.UUID{}, audit.System("test:outward"), kind, pgtype.UUID{}, false, "")
		if !errors.Is(err, restorehold.ErrHeld) {
			t.Fatalf("%s: err = %v, want ErrHeld", kind, err)
		}
	}
}

func TestUnknownKindAndLongNote(t *testing.T) {
	svc := outward.New(nil)
	var inv *outward.InvalidError
	if _, err := svc.SetDisabled(context.Background(), pgtype.UUID{}, audit.System("test:outward"), outward.Kind("nope"), pgtype.UUID{}, true, ""); !errors.As(err, &inv) {
		t.Fatalf("unknown kind: %v", err)
	}
	long := make([]byte, outward.MaxNoteLen+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := svc.SetDisabled(context.Background(), pgtype.UUID{}, audit.System("test:outward"), outward.MCPServer, pgtype.UUID{}, true, string(long)); !errors.As(err, &inv) {
		t.Fatalf("long note: %v", err)
	}
}

func TestDeclaredState(t *testing.T) {
	if !(outward.State{Disabled: true, By: "secret wiki"}).Declared() {
		t.Fatal("a Secret's disable is declared")
	}
	if (outward.State{Disabled: true, By: "user 1"}).Declared() || (outward.State{By: "secret wiki"}).Declared() {
		t.Fatal("an admin's disable, or no disable, is not declared")
	}
}
