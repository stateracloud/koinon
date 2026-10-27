// SPDX-License-Identifier: Apache-2.0

// Package approvalscope finds the scoped approval that lets an ask-tier call run without a new
// decision (plan item AP1; ADR 0042 amendment, ADR 0065 decision 10a).
//
// A scope is exact on agent, connector, tool and target; a task scope also on the task. There are
// no wildcards in v0.1. The call's arguments are not part of a scope: they still go through every
// check the tool server and the executor make, and a call that could push and run code (CI or
// automation files, scripts, dependency manifests, binary or executable files) is never covered
// by a scope, so a person still reads it.
//
// The package only reads; scopes are created by approvals.Service.Approve and revoked there.
package approvalscope

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Limits on how long a scope can last.
const (
	// MaxAgentScope is the longest an agent scope may run.
	MaxAgentScope = 365 * 24 * time.Hour
	// MaxTaskScope is the longest a task scope may run (and its default).
	MaxTaskScope = 30 * 24 * time.Hour
)

// Match returns the active scope that covers the call, if there is one. q may be a transaction.
func Match(ctx context.Context, q *store.Queries, org, agent, task pgtype.UUID, connector, tool, target string, now time.Time) (store.ApprovalScope, bool, error) {
	sc, err := q.FindActiveApprovalScope(ctx, store.FindActiveApprovalScopeParams{
		OrgID: org, AgentID: agent, Connector: connector, Tool: tool, Target: target,
		TaskID: task, Now: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ApprovalScope{}, false, nil
	}
	if err != nil {
		return store.ApprovalScope{}, false, err
	}
	return sc, true, nil
}

// Payload is what the run's timeline records about the scope that allowed a call.
func Payload(sc store.ApprovalScope) map[string]any {
	p := map[string]any{
		"scope_id": sc.ID.String(), "scope_kind": string(sc.Kind), "scope_expires_at": sc.ExpiresAt.Time,
		"scope_created_by": sc.CreatedBy.String(),
	}
	if sc.TaskID.Valid {
		p["scope_task_id"] = sc.TaskID.String()
	}
	return p
}
