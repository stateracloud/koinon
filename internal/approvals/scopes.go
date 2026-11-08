// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/approvalscope"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// ScopeKind is how far an approval reaches (ADR 0042 amendment, plan item AP1).
type ScopeKind string

// The scope kinds. ScopeOnce (also the zero value) is today's behaviour: this exact call.
const (
	ScopeOnce  ScopeKind = ""
	ScopeTask  ScopeKind = "task"
	ScopeAgent ScopeKind = "agent"
)

// Scope widens an approval. Until is when a task or agent scope ends: required for ScopeAgent
// (at most one year ahead); a task scope defaults to, and may not exceed, 30 days.
type Scope struct {
	Kind  ScopeKind
	Until time.Time
}

// Errors of scoped approvals.
var (
	// ErrScopeInvalid: the kind or date of the scope is not acceptable.
	ErrScopeInvalid = errors.New("the approval scope is not valid")
	// ErrScopeNotAllowed: this request cannot be approved with a scope wider than once.
	ErrScopeNotAllowed = errors.New("a scope wider than once is not allowed for a request that needs two approvers or that Koinon made itself")
	// ErrScopeNotFound: no such scope.
	ErrScopeNotFound = errors.New("approval scope not found")
)

// Task event and audit action names of scopes.
const (
	AuditScopeCreate = "approval_scope.create"
	AuditScopeRevoke = "approval_scope.revoke"
)

func (sc Scope) normalize(now time.Time) (Scope, error) {
	switch sc.Kind {
	case ScopeOnce:
		return Scope{}, nil
	case ScopeTask:
		max := now.Add(approvalscope.MaxTaskScope)
		if sc.Until.IsZero() {
			sc.Until = max
		}
		if !sc.Until.After(now) || sc.Until.After(max) {
			return Scope{}, errors.Join(ErrScopeInvalid, errors.New("a task scope ends within 30 days"))
		}
	case ScopeAgent:
		if sc.Until.IsZero() || !sc.Until.After(now) || sc.Until.After(now.Add(approvalscope.MaxAgentScope)) {
			return Scope{}, errors.Join(ErrScopeInvalid, errors.New("an agent scope needs an end date in the future, at most a year ahead"))
		}
	default:
		return Scope{}, ErrScopeInvalid
	}
	return sc, nil
}

// createScope records the scope an approval grants, with an audit entry, in the approval's
// transaction. The scope is exact on the approved request's agent, connector, tool and target.
func (s *Service) createScope(ctx context.Context, q *store.Queries, b audit.Batch, r store.ApprovalRequest, approver pgtype.UUID, sc Scope) (store.ApprovalScope, error) {
	p := store.CreateApprovalScopeParams{
		OrgID: r.OrgID, AgentID: r.AgentID, Connector: r.Connector, Tool: r.Tool, Target: r.Target,
		Kind: store.ApprovalScopeKindAgent, ExpiresAt: pgtype.Timestamptz{Time: sc.Until, Valid: true},
		CreatedBy: approver, SourceApprovalID: r.ID, CallBinding: r.CallBinding,
	}
	if sc.Kind == ScopeTask {
		p.Kind, p.TaskID = store.ApprovalScopeKindTask, r.TaskID
	}
	row, err := q.CreateApprovalScope(ctx, p)
	if err != nil {
		return store.ApprovalScope{}, err
	}
	return row, auditScope(ctx, q, b, row, AuditScopeCreate, audit.User(approver))
}

func auditScope(ctx context.Context, q *store.Queries, b audit.Batch, sc store.ApprovalScope, action string, actor audit.Actor) error {
	after := map[string]any{
		"kind": string(sc.Kind), "agentId": sc.AgentID.String(), "connector": sc.Connector, "tool": sc.Tool,
		"target": sc.Target, "expiresAt": sc.ExpiresAt.Time, "sourceApprovalId": sc.SourceApprovalID.String(),
	}
	if sc.TaskID.Valid {
		after["taskId"] = sc.TaskID.String()
	}
	b.Org(audit.Event{
		Org: sc.OrgID, Actor: actor, Action: action, TargetKind: "approval_scope", TargetID: sc.ID.String(), After: after,
	})
	return nil
}

// ListScopes returns the agent's scopes that are active now.
func (s *Service) ListScopes(ctx context.Context, org, agent pgtype.UUID) ([]store.ApprovalScope, error) {
	return s.q.ListActiveApprovalScopesForAgent(ctx, store.ListActiveApprovalScopesForAgentParams{
		OrgID: org, AgentID: agent, Now: pgtype.Timestamptz{Time: s.now(), Valid: true},
	})
}

// RevokeScope ends a scope at once and audits it. Revoking a revoked scope returns it as it is.
// Calls already approved by it keep running; the agent's next matching call parks again.
func (s *Service) RevokeScope(ctx context.Context, org, id, by pgtype.UUID) (store.ApprovalScope, error) {
	var out store.ApprovalScope
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		row, err := q.RevokeApprovalScope(ctx, store.RevokeApprovalScopeParams{OrgID: org, ID: id, RevokedBy: by})
		if errors.Is(err, pgx.ErrNoRows) {
			out, err = q.GetApprovalScope(ctx, store.GetApprovalScopeParams{OrgID: org, ID: id})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrScopeNotFound
			}
			return err
		}
		if err != nil {
			return err
		}
		out = row
		return auditScope(ctx, q, b, row, AuditScopeRevoke, audit.User(by))
	})
	return out, err
}
