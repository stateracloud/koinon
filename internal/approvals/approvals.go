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
// same transaction and the ticket moves back from Waiting on you to In progress; agentd then
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/procoduck/koinon/internal/connections"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/githubtools"
	"github.com/procoduck/koinon/internal/scheduler"
)

// Ticket event kinds written here. The tool server writes KindRequested.
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

// Defaults.
const (
	DefaultExecutionLease = 2 * time.Minute
	// DefaultMaxExecutions bounds retries of transient GitHub errors.
	DefaultMaxExecutions = 3
)

// Options configures a Service.
type Options struct {
	// Secrets holds connections' private keys. Without it execution fails.
	Secrets connections.SecretStore
	// GitHub defaults to a client for api.github.com.
	GitHub *github.Client
	// ExecutionLease is how long one executor holds a request. Default
	// DefaultExecutionLease.
	ExecutionLease time.Duration
	// MaxExecutions fails a request whose execution kept hitting transient errors. Default
	// DefaultMaxExecutions.
	MaxExecutions int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Service decides and executes approval requests.
type Service struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	secrets connections.SecretStore
	gh      *github.Client
	lease   time.Duration
	maxExec int
	log     *slog.Logger
}

// New returns a Service.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.GitHub == nil {
		opts.GitHub = github.NewClient(github.Options{})
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
	return &Service{
		pool: pool, q: store.New(pool), secrets: opts.Secrets, gh: opts.GitHub,
		lease: opts.ExecutionLease, maxExec: opts.MaxExecutions, log: opts.Logger,
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

func ticketEvent(ctx context.Context, q *store.Queries, r store.ApprovalRequest, kind string, user pgtype.UUID, extra map[string]any) error {
	p := map[string]any{"approval_id": r.ID.String(), "tool": r.Tool, "target": r.Target, "args_sha256": r.ArgsSha256}
	for k, v := range extra {
		p[k] = v
	}
	ev := store.AppendTicketEventParams{
		OrgID: r.OrgID, TicketID: r.TicketID, Kind: kind, ActorKind: store.TicketActorKindSystem,
		RunID: r.RunID, Payload: mustJSON(p),
	}
	if user.Valid {
		ev.ActorKind, ev.ActorUserID = store.TicketActorKindUser, user
	}
	_, err := q.AppendTicketEvent(ctx, ev)
	return err
}

// Approve approves a pending request whose hash is argsSHA256 and executes it before
// returning. A request already approved with that hash (a replay) is returned as it is, and
// not executed again unless an earlier execution is still due.
func (s *Service) Approve(ctx context.Context, org, id, approver pgtype.UUID, argsSHA256, note string) (store.ApprovalRequest, error) {
	var row store.ApprovalRequest
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockApprovalRequest(ctx, store.LockApprovalRequestParams{OrgID: org, ID: id})
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(argsSHA256), cur.ArgsSha256) {
			return ErrHashMismatch
		}
		switch cur.Status {
		case store.ApprovalStatusApproved, store.ApprovalStatusExecuted, store.ApprovalStatusFailed:
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
		if run.State.Finished() {
			return ErrNotPending // the sweeper cancels it
		}
		row, err = q.DecideApprovalRequest(ctx, store.DecideApprovalRequestParams{
			OrgID: org, ID: id, Status: store.ApprovalStatusApproved, DecidedBy: approver,
			DecisionNote: pgtype.Text{String: note, Valid: note != ""},
		})
		if err != nil {
			return err
		}
		return ticketEvent(ctx, q, row, KindApproved, approver, map[string]any{"note": note})
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
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
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
		if err := ticketEvent(ctx, q, row, KindDenied, approver, map[string]any{"note": note}); err != nil {
			return err
		}
		return ResolveRun(ctx, q, org, row.RunID)
	})
	if err == nil {
		s.log.Info("approval denied", attrs(row)...)
	}
	return row, err
}

// ResolveRun makes a run parked on approvals claimable once none of its requests is open,
// and moves its ticket back from Waiting on you. It locks the run row first; call it in the
// transaction that resolved a request.
func ResolveRun(ctx context.Context, q *store.Queries, org, runID pgtype.UUID) error {
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: runID})
	if err != nil {
		return err
	}
	if run.State != store.RunStateParked || run.ParkReason.RunParkReason != store.RunParkReasonApproval || run.ReadyAt.Valid {
		// Still running: agentd sees the outcome when it ends the turn.
		return nil
	}
	reqs, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: org, RunID: runID})
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if r.Status.Open() {
			return nil
		}
	}
	if _, err := q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: runID}); err != nil {
		return err
	}
	_, err = q.MoveTicketFrom(ctx, store.MoveTicketFromParams{
		OrgID: org, ID: run.TicketID, ToStatus: store.TicketStatusInProgress,
		FromStatuses: []string{string(store.TicketStatusWaitingOnYou)},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
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
	var api *github.APIError
	if errors.As(err, &api) {
		return api.Status >= 400 && api.Status < 500 && api.Status != 429 && api.Status != 408
	}
	return false
}

// Execute executes an approved request it can claim and records the outcome. A request
// that is not approved, or that another executor holds, is returned as it is.
func (s *Service) Execute(ctx context.Context, org, id pgtype.UUID) (store.ApprovalRequest, error) {
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
		if err := ticketEvent(ctx, q, out, kind, pgtype.UUID{}, extra); err != nil {
			return err
		}
		return ResolveRun(ctx, q, org, out.RunID)
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
	Executed, Expired, Canceled, RunsCanceled int
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
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		expired, err := q.ExpirePendingApprovalRequests(ctx, org)
		if err != nil {
			return err
		}
		runs := map[pgtype.UUID]bool{}
		for _, r := range expired {
			if err := ticketEvent(ctx, q, r, KindExpired, pgtype.UUID{}, map[string]any{"expires_at": r.ExpiresAt.Time}); err != nil {
				return err
			}
			runs[r.RunID] = true
		}
		res.Expired += len(expired)
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
		if err := ticketEvent(ctx, q, r, KindCanceled, pgtype.UUID{}, map[string]any{"reason": why}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// cancelExpiredRun cancels a run parked on an expired approval, with run and ticket events,
// cancels its other pending requests, and moves its ticket from Waiting on you to To do.
func (s *Service) cancelExpiredRun(ctx context.Context, q *store.Queries, org, runID pgtype.UUID) (bool, error) {
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: runID})
	if err != nil {
		return false, err
	}
	if run.State != store.RunStateParked {
		// A running run sees the expiry as the outcome when its turn ends.
		return false, nil
	}
	if _, err := scheduler.Cancel(ctx, q, org, runID, scheduler.ReasonApprovalExpired, expiredMessage, scheduler.System); err != nil {
		return false, err
	}
	if _, err := s.cancelPending(ctx, q, org, runID, expiredMessage); err != nil {
		return false, err
	}
	_, err = q.MoveTicketFrom(ctx, store.MoveTicketFromParams{
		OrgID: org, ID: run.TicketID, ToStatus: store.TicketStatusTodo,
		FromStatuses: []string{string(store.TicketStatusWaitingOnYou)},
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
