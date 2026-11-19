// SPDX-License-Identifier: Apache-2.0

// Package pgaudit writes the gateway's audit row of each MCP tool call to Postgres (ADR 0097 decision
// 5): one row in mcp_call_log, with no argument, no result and no token value.
package pgaudit

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// Writer implements mcpgateway.Audit on a pool whose role may insert into mcp_call_log.
type Writer struct {
	q *store.Queries
}

var _ mcpgateway.Audit = (*Writer)(nil)

// New returns a Writer on pool.
func New(pool *pgxpool.Pool) *Writer { return &Writer{q: store.New(pool)} }

// writeTimeout bounds one insert, so a slow database cannot hold a call's goroutine for long.
const writeTimeout = 5 * time.Second

// Record implements mcpgateway.Audit.
func (w *Writer) Record(ctx context.Context, r mcpgateway.AuditRow) error {
	// The call's context may be cancelled already (the agent gave up); the row is still owed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	return w.q.InsertMCPCallLog(ctx, store.InsertMCPCallLogParams{
		OrgSlug: r.Agent.Org, Agent: r.Agent.Name, Target: r.Target, Tool: r.Tool,
		Outcome: string(r.Outcome), Verdict: truncate(r.Verdict, 1024), CredentialKind: r.CredKind,
		DurationMs: int32(min(r.Duration.Milliseconds(), 1<<31-1)), //nolint:gosec // bounded above
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
