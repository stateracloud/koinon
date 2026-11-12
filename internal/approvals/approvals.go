// SPDX-License-Identifier: Apache-2.0

// Package approvals decides, executes and expires ask-tier tool calls (plan item P1; ADRs
// 0008, 0042, 0049).
//
// The GitHub tool server (internal/githubtools) records each call an agent makes as an
// approval request and answers "approval requested" at once; agentd parks the run when the
// turn ends. This package is the other half:
//
//   - Approve binds to the hash of the canonical arguments the approver saw. A different hash
//     is ErrHashMismatch and nothing runs. Approving again with the same hash returns the
//     request as it is.
//   - Execute runs exactly the approved call outside the sandbox, with an installation token
//     narrowed to the one repository and the permissions the tool needs. The request ID is
//     the idempotency key: it is written into what Koinon creates (a commit trailer, a hidden
//     comment marker), and a pre-create lookup finds what an earlier attempt created, so a
//     replay or a crash between the GitHub calls and the database write never creates
//     anything twice.
//   - Deny records the decision; the run resumes with it.
//   - Sweep executes approved requests nobody holds (the Trusted preset's, crashed
//     executors'), expires requests older than their TTL and cancels their parked runs, and
//     cancels requests whose run finished without them.
//
// Whenever the last open request of a parked run is resolved, the park is resolved in the
// same transaction and the task moves back from Waiting on you to In progress; agentd then
// claims the run, loads the session and sends the outcomes as the next prompt. The run row
// lock (store.LockRun) orders this against agentd's park decision, so neither side misses the
// other.
//
// Tokens never leave the github client's cache: they are not logged, stored or put into
// errors.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/restorehold"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/runpark"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Task event kinds written here. The tool server writes KindRequested.
const (
	KindRequested = githubtools.KindApprovalRequested
	KindApproved  = "approval.approved"
	KindDenied    = "approval.denied"
	KindExecuted  = "approval.executed"
	KindFailed    = "approval.failed"
	KindExpired   = "approval.expired"
	KindCanceled  = "approval.canceled"
)

// Errors of Approve and Deny.
var (
	// ErrHashMismatch: the hash is not the request's; nothing was decided or executed.
	ErrHashMismatch = errors.New("approval does not match the requested arguments")
	// ErrNotPending: the request was already decided otherwise, expired or canceled.
	ErrNotPending = errors.New("approval request is no longer pending")
)

// ExecKeys yields an org's gateway executor key (package mcpservers).
type ExecKeys interface {
	Key(ctx context.Context, org pgtype.UUID) (string, error)
}

// Errors of Approve beyond those above (two-approver requests).
var (
	// ErrSameApprover: the second approval must come from a different person than the first.
	ErrSameApprover = errors.New("this request needs two approvers; the second must be a different person than the first")
	// ErrRequesterApproves: the person who created the task may not approve a two-approver request.
	ErrRequesterApproves = errors.New("this request needs two approvers, and the person who created the task cannot be one of them")
)

// KindFirstApproved is the task event of the first of two approvals.
const KindFirstApproved = "approval.first_approved"

// Defaults.
const (
	DefaultExecutionLease = 2 * time.Minute
	// DefaultMaxExecutions bounds retries of transient GitHub errors.
	DefaultMaxExecutions = 3
)

// Options configures a Service.
type Options struct {
	// RestoreHold refuses Approve, Execute and Diff with restorehold.ErrHeld while it holds (issue
	// #462, ADR 0107): an approved call reaches outward (code host, MCP server, saved-work push).
	RestoreHold *restorehold.Guard
	// Keys opens connections' stored secret fields. Without it execution fails.
	Keys connections.Opener
	// GatewayMCPURL is agentgateway's MCP endpoint and ExecKeys the org's executor keys: approved
	// calls to remote MCP servers go through the gateway, which holds their credentials. Without
	// them such a call fails permanently.
	GatewayMCPURL string
	ExecKeys      ExecKeys
	// SavedWorkKeys opens saved work sealed by koinon-agentd (package savedwork). Without it a
	// push of saved work fails permanently.
	SavedWorkKeys *secretstore.Keyring
	// Hosts are the code-host providers, by connector type ID. Without it the shipped providers
	// are used, with GitHub as the client below.
	Hosts *codehost.Registry
	// GitHub is the client of the shipped GitHub provider when Hosts is nil; it defaults to a
	// client for api.github.com.
	GitHub *github.Client
	// DialContext is the egress policy's dial check (catalog.URLPolicy.DialContext). The push
	// writer's fetch, ref listing and push dial through it and refuse redirects. Nil dials
	// unchecked, for tests.
	DialContext gitpush.DialFunc
	// ExecutionLease is how long one executor holds a request. Default
	// DefaultExecutionLease.
	ExecutionLease time.Duration
	// MaxExecutions fails a request whose execution kept hitting transient errors. Default
	// DefaultMaxExecutions.
	MaxExecutions int
	// Now is the clock; it defaults to time.Now. Scope expiry is measured against it.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// OnMCPResult records the result of an approved MCP call in the requesting agent's leak-guard
	// session (internal/mcpguard: its legs and fingerprints), since the executor's own call is not
	// the agent's. If it fails the result is withheld and the request fails: a result the guard did
	// not see could leave the org later unchecked. Nil records nothing.
	OnMCPResult func(ctx context.Context, row store.ApprovalRequest, srv store.McpServer, result json.RawMessage) error
}

// Service decides and executes approval requests.
type Service struct {
	pool      *pgxpool.Pool
	q         *store.Queries
	keys      connections.Opener
	gwURL     string
	execKey   ExecKeys
	savedKeys *secretstore.Keyring
	hosts     *codehost.Registry
	dial      gitpush.DialFunc
	lease     time.Duration
	maxExec   int
	now       func() time.Time
	log       *slog.Logger
	hold      *restorehold.Guard
	onMCP     func(ctx context.Context, row store.ApprovalRequest, srv store.McpServer, result json.RawMessage) error
}

// New returns a Service.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.Hosts == nil {
		opts.Hosts = providers.New(opts.GitHub)
	}
	if opts.ExecutionLease <= 0 {
		opts.ExecutionLease = DefaultExecutionLease
	}
	if opts.MaxExecutions <= 0 {
		opts.MaxExecutions = DefaultMaxExecutions
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		now:  opts.Now,
		pool: pool, q: store.New(pool), keys: opts.Keys, gwURL: opts.GatewayMCPURL, execKey: opts.ExecKeys, savedKeys: opts.SavedWorkKeys, hosts: opts.Hosts, dial: opts.DialContext,
		lease: opts.ExecutionLease, maxExec: opts.MaxExecutions, log: opts.Logger, hold: opts.RestoreHold, onMCP: opts.OnMCPResult,
	}
}

func attrs(r store.ApprovalRequest) []any {
	return []any{"org", r.OrgID.String(), "approval", r.ID.String(), "tool", r.Tool, "target", r.Target, "run", r.RunID.String()}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only maps of plain values are passed
	}
	return b
}

func taskEvent(ctx context.Context, q *store.Queries, r store.ApprovalRequest, kind string, user pgtype.UUID, extra map[string]any) error {
	p := map[string]any{"approval_id": r.ID.String(), "tool": r.Tool, "target": r.Target, "args_sha256": r.ArgsSha256}
	for k, v := range extra {
		p[k] = v
	}
	ev := store.AppendTaskEventParams{
		OrgID: r.OrgID, TaskID: r.TaskID, Kind: kind, ActorKind: store.TaskActorKindSystem,
		RunID: r.RunID, Payload: mustJSON(p),
	}
	if user.Valid {
		ev.ActorKind, ev.ActorUserID = store.TaskActorKindUser, user
	}
	_, err := q.AppendTaskEvent(ctx, ev)
	return err
}

// auditDecision records a decision on an approval request in the audit log, in the transaction
// that made it (ADR 0005, 0023: who requested, who approved). actor is the approver, or Koinon
// itself for an expiry. The request's arguments are not copied: their hash identifies them.
func auditDecision(ctx context.Context, q *store.Queries, b audit.Batch, r store.ApprovalRequest, action string, actor audit.Actor, note string) error {
	after := map[string]any{
		"status": string(r.Status), "tool": r.Tool, "target": r.Target, "argsSha256": r.ArgsSha256,
		"runId": r.RunID.String(), "taskId": r.TaskID.String(),
	}
	if note != "" {
		after["note"] = note
	}
	b.Org(audit.Event{
		Org: r.OrgID, Actor: actor, Action: action, TargetKind: "approval", TargetID: r.ID.String(), After: after,
	})
	return nil
}

// Approve approves a pending request whose hash is argsSHA256 and executes it before
// returning. A request already approved with that hash (a replay) is returned as it is, and
// not executed again unless an earlier execution is still due.
func (s *Service) Approve(ctx context.Context, org, id, approver pgtype.UUID, argsSHA256, note string) (store.ApprovalRequest, error) {
	return s.ApproveScoped(ctx, org, id, approver, argsSHA256, note, Scope{})
}

// ApproveScoped is Approve with a scope: a zero Scope (or ScopeOnce) approves this call only; a
// wider one also records an approval scope for the agent (see Scope). The caller checks that the
// approver is an org admin or owner, as for any approval.
func (s *Service) ApproveScoped(ctx context.Context, org, id, approver pgtype.UUID, argsSHA256, note string, scope Scope) (store.ApprovalRequest, error) {
	// An approval runs the call at once; a held install does not act outward, so it does not approve
	// either (Deny stays open).
	if err := s.hold.Err(); err != nil {
		return store.ApprovalRequest{}, err
	}
	scope, err := scope.normalize(s.now())
	if err != nil {
		return store.ApprovalRequest{}, err
	}
	var row store.ApprovalRequest
	_, err = audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.LockApprovalRequest(ctx, store.LockApprovalRequestParams{OrgID: org, ID: id})
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(argsSHA256), cur.ArgsSha256) {
			return ErrHashMismatch
		}
		switch cur.Status {
		case store.ApprovalStatusApproved, store.ApprovalStatusExecuted, store.ApprovalStatusFailed:
			if scope.Kind != ScopeOnce {
				// A replay cannot widen the approval: the admin would believe a standing approval exists.
				return ErrNotPending
			}
			row = cur // a replay: the same decision again
			return nil
		case store.ApprovalStatusPending:
		default:
			return ErrNotPending
		}
		run, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: cur.RunID})
		if err != nil {
			return err
		}
		if run.State.Finished() && !cur.System {
			return ErrNotPending // the sweeper cancels it
		}
		if cur.ApprovalsRequired < 2 && cur.Connector == mcpask.ConnectorID {
			// The tool's flag is read now, not only when the request was recorded: turning it on
			// covers requests that are already pending.
			var b mcpservers.CallBinding
			var sid pgtype.UUID
			if json.Unmarshal(cur.CallBinding, &b) == nil && sid.Scan(b.ServerID) == nil {
				if tool, err := q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: org, ServerID: sid, Name: cur.Tool}); err == nil && tool.RequiresTwoApprovers {
					if cur, err = q.RaiseApprovalsRequired(ctx, store.RaiseApprovalsRequiredParams{OrgID: org, ID: id}); err != nil {
						return err
					}
				}
			}
		}
		if scope.Kind != ScopeOnce && (cur.ApprovalsRequired >= 2 || cur.System) {
			// The second person of a two-approver request, and Koinon's own requests, cannot be waived.
			return ErrScopeNotAllowed
		}
		if cur.ApprovalsRequired >= 2 {
			// Two approvers: distinct people, neither the one who created the task (they asked for
			// the work; a second pair of eyes is the point). The role check is the caller's.
			t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: org, ID: cur.TaskID})
			if err != nil {
				return err
			}
			if t.CreatedByUserID.Valid && t.CreatedByUserID == approver {
				return ErrRequesterApproves
			}
			if !cur.FirstApprover.Valid {
				row, err = q.SetApprovalFirstApprover(ctx, store.SetApprovalFirstApproverParams{OrgID: org, ID: id, Approver: approver})
				if err != nil {
					return err
				}
				if err := taskEvent(ctx, q, row, KindFirstApproved, approver, map[string]any{"note": note}); err != nil {
					return err
				}
				return auditDecision(ctx, q, b, row, "approval.first_approve", audit.User(approver), note)
			}
			if cur.FirstApprover == approver {
				return ErrSameApprover
			}
		}
		row, err = q.DecideApprovalRequest(ctx, store.DecideApprovalRequestParams{
			OrgID: org, ID: id, Status: store.ApprovalStatusApproved, DecidedBy: approver,
			DecisionNote: pgtype.Text{String: note, Valid: note != ""},
		})
		if err != nil {
			return err
		}
		extra := map[string]any{"note": note}
		if scope.Kind != ScopeOnce {
			sc, err := s.createScope(ctx, q, b, row, approver, scope)
			if err != nil {
				return err
			}
			extra["scope_id"], extra["scope_kind"], extra["scope_expires_at"] = sc.ID.String(), string(sc.Kind), sc.ExpiresAt.Time
		}
		if err := taskEvent(ctx, q, row, KindApproved, approver, extra); err != nil {
			return err
		}
		return auditDecision(ctx, q, b, row, "approval.approve", audit.User(approver), note)
	})
	if err != nil {
		return store.ApprovalRequest{}, err
	}
	if row.Status == store.ApprovalStatusApproved {
		s.log.Info("approval approved", attrs(row)...)
		// The approver's request may go away; the execution must not stop halfway.
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.lease)
		defer cancel()
		return s.Execute(ectx, org, id)
	}
	return row, nil
}

// Deny denies a pending request; its run resumes with the denial.
func (s *Service) Deny(ctx context.Context, org, id, approver pgtype.UUID, note string) (store.ApprovalRequest, error) {
	var row store.ApprovalRequest
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.LockApprovalRequest(ctx, store.LockApprovalRequestParams{OrgID: org, ID: id})
		if err != nil {
			return err
		}
		if cur.Status == store.ApprovalStatusDenied {
			row = cur
			return nil
		}
		if cur.Status != store.ApprovalStatusPending {
			return ErrNotPending
		}
		row, err = q.DecideApprovalRequest(ctx, store.DecideApprovalRequestParams{
			OrgID: org, ID: id, Status: store.ApprovalStatusDenied, DecidedBy: approver,
			DecisionNote: pgtype.Text{String: note, Valid: note != ""},
		})
		if err != nil {
			return err
		}
		if err := taskEvent(ctx, q, row, KindDenied, approver, map[string]any{"note": note}); err != nil {
			return err
		}
		if err := auditDecision(ctx, q, b, row, "approval.deny", audit.User(approver), note); err != nil {
			return err
		}
		if row.System {
			return nil
		}
		return runpark.Resolve(ctx, q, org, row.RunID)
	})
	if err == nil {
		s.log.Info("approval denied", attrs(row)...)
	}
	return row, err
}

// permanentError is an execution failure that a retry would not fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// isPermanent classifies an execution error: GitHub's 4xx answers (other than rate limits)
// and Koinon's own refusals are permanent; network errors and 5xx are retried.
func isPermanent(err error) bool {
	var p permanentError
	if errors.As(err, &p) {
		return true
	}
	if status := codehost.Status(err); status != 0 {
		return status >= 400 && status < 500 && status != 429 && status != 408
	}
	return false
}

// Execute executes an approved request it can claim and records the outcome. A request
// that is not approved, or that another executor holds, is returned as it is.
func (s *Service) Execute(ctx context.Context, org, id pgtype.UUID) (store.ApprovalRequest, error) {
	if err := s.hold.Err(); err != nil {
		return store.ApprovalRequest{}, err
	}
	row, err := s.q.ClaimApprovalExecution(ctx, store.ClaimApprovalExecutionParams{
		OrgID: org, ID: id, LeaseSeconds: int32(s.lease / time.Second), //nolint:gosec // bounded by configuration
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: org, ID: id})
	}
	if err != nil {
		return store.ApprovalRequest{}, err
	}
	log := s.log.With(attrs(row)...)
	log.Info("executing approved call", "execution", row.Executions)
	result, execErr := s.execute(ctx, row)
	if execErr != nil && !isPermanent(execErr) && int(row.Executions) < s.maxExec {
		// Leave it approved: the sweeper retries once the lease has passed.
		log.Warn("executing the approved call failed; it is retried", "execution", row.Executions, "err", execErr)
		return row, nil
	}
	params := store.CompleteApprovalExecutionParams{OrgID: org, ID: id, Executions: row.Executions, Status: store.ApprovalStatusExecuted}
	kind, extra := KindExecuted, map[string]any{"result": result}
	if execErr != nil {
		params.Status = store.ApprovalStatusFailed
		params.Error = pgtype.Text{String: truncate(execErr.Error(), 2000), Valid: true}
		kind, extra = KindFailed, map[string]any{"error": params.Error.String}
	} else {
		params.Result = mustJSON(result)
	}
	var out store.ApprovalRequest
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		out, err = q.CompleteApprovalExecution(ctx, params)
		if err != nil {
			return err
		}
		if err := taskEvent(ctx, q, out, kind, pgtype.UUID{}, extra); err != nil {
			return err
		}
		if out.System {
			// Koinon's own request: the run is not waiting for it. The sealed files go once pushed.
			if execErr == nil {
				if a, err := savedwork.DecodePushArgs(out.ArgsJson); err == nil {
					var id pgtype.UUID
					if id.Scan(a.SavedWorkID) == nil {
						_, err = q.MarkSavedWorkPushed(ctx, store.MarkSavedWorkPushedParams{OrgID: org, ID: id})
						return err
					}
				}
			}
			return nil
		}
		return runpark.Resolve(ctx, q, org, out.RunID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Another executor took over after our lease ran out; its outcome counts.
		return s.q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: org, ID: id})
	}
	if err != nil {
		return store.ApprovalRequest{}, err
	}
	if execErr != nil {
		log.Warn("approved call failed", "err", execErr)
	} else {
		log.Info("approved call executed", "result", result)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SweepResult counts what one sweep did.
type SweepResult struct {
	Executed, Expired, Canceled, RunsCanceled, SavedWorkExpired int
}

// Sweep does the periodic work in every org: execute approved requests nobody holds,
// expire stale ones and cancel their runs, and cancel requests whose run finished. Several
// API replicas may sweep at once; every step is a compare-and-set.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	orgs, err := s.q.ListOrgIDs(ctx)
	if err != nil {
		return res, err
	}
	var firstErr error
	for _, org := range orgs {
		if err := s.sweepOrg(ctx, org, &res); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("sweep org %s: %w", org.String(), err)
		}
	}
	return res, firstErr
}

const expiredMessage = "approval request expired: nobody decided within 7 days"

func (s *Service) sweepOrg(ctx context.Context, org pgtype.UUID, res *SweepResult) error {
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		expired, err := q.ExpirePendingApprovalRequests(ctx, org)
		if err != nil {
			return err
		}
		runs := map[pgtype.UUID]bool{}
		for _, r := range expired {
			if err := taskEvent(ctx, q, r, KindExpired, pgtype.UUID{}, map[string]any{"expires_at": r.ExpiresAt.Time}); err != nil {
				return err
			}
			if err := auditDecision(ctx, q, b, r, "approval.expire", audit.System("koinon-api:approvals"), ""); err != nil {
				return err
			}
			if !r.System { // Koinon's own request: no run waits for it
				runs[r.RunID] = true
			}
		}
		res.Expired += len(expired)
		// Saved work is kept for 14 days; the sealed files go with it.
		gone, err := q.ExpireSavedWork(ctx, org)
		if err != nil {
			return err
		}
		res.SavedWorkExpired += len(gone)
		// Upload nonces only matter while their token could be good.
		if _, err := q.DeleteOldUploadNonces(ctx, org); err != nil {
			return err
		}
		parked, err := q.ListExpiredParkedRuns(ctx, org)
		if err != nil {
			return err
		}
		for _, r := range parked {
			if r.ParkReason.RunParkReason == store.RunParkReasonApproval {
				runs[r.ID] = true
			}
		}
		for id := range runs {
			canceled, err := s.cancelExpiredRun(ctx, q, org, id)
			if err != nil {
				return err
			}
			if canceled {
				res.RunsCanceled++
			}
		}
		orphans, err := q.ListPendingApprovalRequestsOfFinishedRuns(ctx, org)
		if err != nil {
			return err
		}
		done := map[pgtype.UUID]bool{}
		for _, o := range orphans {
			if done[o.RunID] {
				continue
			}
			done[o.RunID] = true
			n, err := s.cancelPending(ctx, q, org, o.RunID, "the run ended before a decision")
			if err != nil {
				return err
			}
			res.Canceled += n
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.hold.Held() {
		return nil // the hold executes nothing; the due calls wait for the release
	}
	due, err := s.q.ListExecutableApprovalRequests(ctx, store.ListExecutableApprovalRequestsParams{OrgID: org, MaxRequests: 20})
	if err != nil {
		return err
	}
	for _, r := range due {
		out, err := s.Execute(ctx, org, r.ID)
		if err != nil {
			return err
		}
		if !out.Status.Open() {
			res.Executed++
		}
	}
	return nil
}

func (s *Service) cancelPending(ctx context.Context, q *store.Queries, org, runID pgtype.UUID, why string) (int, error) {
	rows, err := q.CancelPendingApprovalRequestsForRun(ctx, store.CancelPendingApprovalRequestsForRunParams{OrgID: org, RunID: runID})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := taskEvent(ctx, q, r, KindCanceled, pgtype.UUID{}, map[string]any{"reason": why}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// cancelExpiredRun cancels a run parked on an expired approval, with run and task events,
// cancels its other pending requests, and moves its task from Waiting on you to To do.
func (s *Service) cancelExpiredRun(ctx context.Context, q *store.Queries, org, runID pgtype.UUID) (bool, error) {
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: runID})
	if err != nil {
		return false, err
	}
	if run.State != store.RunStateParked {
		// A running run sees the expiry as the outcome when its turn ends.
		return false, nil
	}
	if run.ParkReason.RunParkReason == store.RunParkReasonInputRequired {
		// The run is parked on an open question too (ADR 0065 TK3: only parks with reason
		// approval are canceled here). The approval is expired, so the run resumes with
		// "approval expired" once its questions are resolved.
		return false, runpark.Resolve(ctx, q, org, runID)
	}
	if _, err := runoutcome.CancelUnowned(ctx, q, org, runID, runoutcome.Cancellation{
		Reason: runoutcome.ReasonApprovalExpired, Message: expiredMessage, Actor: runoutcome.System,
	}); err != nil {
		return false, err
	}
	if _, err := s.cancelPending(ctx, q, org, runID, expiredMessage); err != nil {
		return false, err
	}
	_, err = q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
		OrgID: org, ID: run.TaskID, ToState: store.TaskStateSubmitted,
		FromStates: []string{string(store.TaskStateInputRequired)},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	s.log.Info("parked run canceled: approval expired", "org", org.String(), "run", runID.String())
	return true, nil
}

// Sweeper runs Sweep every interval until ctx ends.
func (s *Service) Sweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, err := s.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("approval sweep failed", "err", err)
		} else if res != (SweepResult{}) {
			s.log.Info("approval sweep", "executed", res.Executed, "expired", res.Expired,
				"canceled", res.Canceled, "runs_canceled", res.RunsCanceled)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hostName is how messages name the code host of a connector type.
func (s *Service) hostName(connector string) string {
	if p, ok := s.hosts.Get(connector); ok {
		return p.Capabilities().DisplayName
	}
	return "the code host"
}
