// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// A held install dials no MCP server: the preflight of Create refuses before it runs, and before
// anything is written (issue #462, ADR 0107).
func TestPreflightRefusesWhileHeld(t *testing.T) {
	calls := 0
	svc := mcpservers.New(nil, nil, nil).
		WithPreflight(func(context.Context, mcpservers.PreflightRequest) error { calls++; return nil }).
		WithRestoreHold(restorehold.NewGuard(true))
	_, err := svc.Create(context.Background(), pgtype.UUID{}, audit.Actor{}, pgtype.UUID{},
		mcpservers.Input{Name: "docs", URL: "https://203.0.113.10/mcp", Transport: "streamable-http"})
	if !errors.Is(err, restorehold.ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
	if calls != 0 {
		t.Fatalf("the preflight ran %d times", calls)
	}
}
