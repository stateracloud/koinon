// SPDX-License-Identifier: Apache-2.0

package modelcheck_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/restorehold"
)

type countingChat struct{ calls int }

func (c *countingChat) Complete(context.Context, pgtype.UUID, string, modelformat.Format, string) (string, error) {
	c.calls++
	return "", nil
}

// A held install makes no model call: Check refuses before it reads anything (issue #462, ADR 0107).
func TestCheckRefusesWhileHeld(t *testing.T) {
	chat := &countingChat{}
	svc := modelcheck.New(nil, chat, prices.Resolver{}).WithRestoreHold(restorehold.NewGuard(true))
	_, err := svc.Check(context.Background(), pgtype.UUID{}, pgtype.UUID{}, "m", audit.Actor{}, pgtype.UUID{})
	if !errors.Is(err, restorehold.ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
	if chat.calls != 0 {
		t.Fatalf("the gateway was called %d times", chat.calls)
	}
}
