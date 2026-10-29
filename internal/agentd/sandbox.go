// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/runreply"
)

// waitSandbox waits until the agent's sandbox is ready and returns its row: the workspace, the
// name and the home generation the harness session must belong to.
//
// koinon-reconciler is the one source of sandbox state: it renders the sandbox, records what
// it observes in agent_sandboxes and is the only component that starts or stops it. agentd
// reads that row. A stopped sandbox (idle stop, ADR 0049) is started by the reconciler because
// this run, now starting, is demand for it (GetAgentRunActivity); while the sandbox is not
// ready agentd NOTIFYs the reconciler every ReconcileNudge, so a pass follows within a second
// rather than a resync interval (a takeover, for example, changes no run state and so fires no
// trigger). A missing row is waited for, since the reconciler creates it when the agent is
// activated. A run whose sandbox is not ready within the org's queue wait (SandboxTimeout by
// default) goes back to the queue (requeued) instead of failing, so a burst beyond the cluster's
// capacity shows up as waiting, not as failed runs (ADR 0066, plan item LT1). The requeue is
// bounded: a run that has waited longer than the org's sandbox wait cap in total, across requeues
// (runs.sandbox_wait_started_at), fails with the class sandbox_unavailable and names the cause
// (ADR 0066 amendment 2026-10-28).
func (r *runner) waitSandbox(ctx context.Context) (store.AgentSandbox, error) {
	now := r.p.cfg.Now
	started := now()
	defer func() { r.p.cfg.Metrics.SandboxWait.Observe(now().Sub(started).Seconds()) }()
	// The org's queue wait is read once the first poll finds the sandbox not ready, so the common
	// path (a ready sandbox) costs no extra query. The deadline counts from the start of the wait.
	wait := r.p.cfg.SandboxTimeout
	var deadline time.Time
	var nudged time.Time
	var last string
	// The latest row observed, for naming the cause at the cap.
	var lastReason, lastMessage string
	observed := false
	var waitStart time.Time
	var capd time.Duration
	for {
		sb, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		if err == nil {
			lastReason, lastMessage, observed = sb.Reason, sb.Message, true
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			last = "not rendered yet"
		case err != nil:
			if ctx.Err() != nil {
				return store.AgentSandbox{}, ctx.Err()
			}
			last = err.Error()
		case agentconfig.IsEphemeral(r.vcfg) && sb.SandboxName != r.taskSandboxName() &&
			sb.SandboxName != execution.AgentSandboxName(r.run.OrgID.Bytes, r.run.AgentID.Bytes):
			// An ephemeral agent has one sandbox per task (ADR 0065 decision 13c). The row still
			// names the previous task's, which the reconciler is deleting before it renders this
			// task's. Never run in it. The source of truth for ephemeral is the agent's ACTIVE
			// version, which the reconciler reads; the run's pinned version (vcfg) can be older. If
			// a persistent version was activated since, the row moves back to the agent's own
			// sandbox, which is accepted here so the run does not wait for a task sandbox that
			// will never come.
			last = "waiting for the task's own sandbox"
		case sb.State == store.SandboxStateReady && sb.UnhealthyAt.Valid:
			// Found stuck: the reconciler recreates it (ADR 0054).
			last = "recreating the stuck sandbox"
		case sb.State == store.SandboxStateReady:
			// A sandbox that was ready since is not one that "never became ready": every run of the
			// agent starts a fresh wait the next time it finds the sandbox not ready.
			if _, err := r.q.ClearSandboxWaitForAgent(ctx, store.ClearSandboxWaitForAgentParams{
				OrgID: r.run.OrgID, AgentID: r.run.AgentID,
			}); err != nil && ctx.Err() == nil {
				r.log.Warn("clearing the sandbox wait failed", "err", err)
			}
			return sb, nil
		case sb.State == store.SandboxStateError && r.agentHealthy(ctx):
			// The reconciler recreates a failed sandbox once no run is active (and gives up by
			// marking the agent unhealthy), so wait for it, within SandboxTimeout.
			last = "error"
			if sb.Message != "" {
				last += ": " + sb.Message
			}
		case sb.State == store.SandboxStateError, sb.State == store.SandboxStateDeleted,
			sb.State == store.SandboxStateRetained:
			reason := "sandbox " + string(sb.State)
			if sb.Phase != "" {
				reason += " (" + sb.Phase + ")"
			}
			if sb.Message != "" {
				reason += ": " + sb.Message
			}
			return store.AgentSandbox{}, &failure{reason: reason}
		default: // pending, starting, stopping, stopped, deleting
			// Deleting is a recreation in progress: for a changed spec, a stuck sandbox (ADR 0054)
			// or one found in Error or Completed. The next pass creates the new sandbox, so the
			// run waits for it, within SandboxTimeout, instead of failing.
			last = string(sb.State)
		}
		if deadline.IsZero() {
			capd = r.p.cfg.SandboxWaitCap
			if s, err := orgsettings.Get(ctx, r.q, r.run.OrgID, orgsettings.Settings{QueueWait: wait, SandboxWaitCap: capd}); err == nil {
				wait, capd = s.QueueWait, s.SandboxWaitCap
			} else if ctx.Err() == nil {
				r.log.Warn("reading the org's settings failed; using the default queue wait", "err", err)
			}
			waitStart = started
			switch ws, err := r.q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{
				OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, Started: pgtype.Timestamptz{Time: started, Valid: true},
			}); {
			case errors.Is(err, pgx.ErrNoRows):
				return store.AgentSandbox{}, errFenced
			case err != nil:
				if ctx.Err() != nil {
					return store.AgentSandbox{}, ctx.Err()
				}
				r.log.Warn("recording the start of the sandbox wait failed; counting from now", "err", err)
			case ws.Valid:
				waitStart = ws.Time
			}
			// Every claim gets one nudge to the reconciler and one poll after it before the cap can
			// fail it: a run whose cap ran out while it was queued must not fail on its first poll.
			deadline = laterOf(earlierOf(started.Add(wait), waitStart.Add(capd)), started.Add(r.p.cfg.ReconcileNudge+r.p.cfg.Poll))
		}
		if now().Sub(nudged) >= r.p.cfg.ReconcileNudge {
			if nudged.IsZero() {
				r.log.Info("waiting for the reconciler to make the sandbox ready", "state", last)
			}
			nudged = now()
			if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil && ctx.Err() == nil {
				r.log.Warn("notifying the reconciler failed", "err", err)
			}
		}
		if now().After(deadline) {
			if waited := now().Sub(waitStart); waited >= capd {
				cause, detail := execution.ClassifySandboxWait(lastReason, lastMessage), last
				if !observed {
					cause, detail = execution.WaitCauseUnknown, "the sandbox was never rendered"
				}
				return store.AgentSandbox{}, &failure{
					reason:  fmt.Sprintf("the sandbox never became ready in %s: %s (%s)", runreply.Human(capd), cause.Describe(), detail),
					class:   classSandboxUnavailable,
					sandbox: &sandboxUnavailable{cause: cause, detail: detail, waited: waited, cap: capd},
				}
			}
			reason := fmt.Sprintf("sandbox not ready after %s (%s)", wait, last)
			// Only a starting run can go back to the queue (RequeueRun). A run taken over while
			// running has no queue to return to, so it fails, as it did before LT1.
			if r.run.State != store.RunStateStarting {
				return store.AgentSandbox{}, &failure{reason: reason, class: classTransient}
			}
			return store.AgentSandbox{}, &requeued{reason: reason}
		}
		if !sleep(ctx, r.p.cfg.Poll) {
			return store.AgentSandbox{}, ctx.Err()
		}
	}
}

func earlierOf(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// taskSandboxName is the sandbox the run's task has when its agent is ephemeral.
func (r *runner) taskSandboxName() string {
	return execution.TaskSandboxName(r.run.OrgID.Bytes, r.run.AgentID.Bytes, r.run.TaskID.Bytes)
}

// mkdir creates dir in the sandbox. Harnesses refuse a session whose directory is missing.
func (r *runner) mkdir(ctx context.Context, workspace, name, dir string) error {
	_, code, stderr, err := r.sandboxExec(ctx, workspace, name, []string{"/bin/mkdir", "-p", dir}, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return &failure{reason: fmt.Sprintf("mkdir %s exited with %d: %s", dir, code, stderr)}
	}
	return nil
}

// sandboxExec runs a short command in the sandbox with stdin as its input and returns its
// output, exit code and the tail of its standard error.
func (r *runner) sandboxExec(ctx context.Context, workspace, name string, cmd []string, stdin []byte) ([]byte, int, string, error) {
	return r.sandboxExecLimit(ctx, workspace, name, cmd, stdin, time.Minute, 1<<20)
}

// sandboxExecLimit is sandboxExec with its own timeout and cap on the output it reads.
func (r *runner) sandboxExecLimit(ctx context.Context, workspace, name string, cmd []string, stdin []byte, timeout time.Duration, limit int64) ([]byte, int, string, error) {
	var stderr tail
	proc, err := r.p.backend.Exec(ctx, workspace, name, execution.ExecRequest{
		Command: cmd, Stderr: &stderr, Timeout: timeout,
	})
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = proc.Close() }()
	werr := make(chan error, 1)
	go func() {
		_, err := proc.Stdin().Write(stdin)
		werr <- errors.Join(err, proc.Stdin().Close())
	}()
	out, _ := io.ReadAll(io.LimitReader(proc.Stdout(), limit))
	code, err := proc.Wait()
	if err != nil {
		return nil, 0, "", err
	}
	if err := <-werr; err != nil && code == 0 {
		return nil, 0, "", err
	}
	return out, code, stderr.String(), nil
}

// closeProcess ends a harness: end-of-file on stdin first, so it can exit cleanly, then the
// stream.
func closeProcess(p execution.Process) {
	_ = p.Stdin().Close()
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	_ = p.Close()
}

// tail keeps the last bytes written to it, for error reasons. Harness stderr can be long.
type tail struct {
	mu sync.Mutex
	b  []byte
}

const tailSize = 2048

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = append([]byte(nil), t.b[len(t.b)-tailSize:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// agentHealthy reports whether the agent is not marked unhealthy (ADR 0054). When it cannot tell,
// it says no, so the run fails at once as it always did.
func (r *runner) agentHealthy(ctx context.Context) bool {
	a, err := r.q.GetAgent(ctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
	return err == nil && !a.UnhealthyAt.Valid
}

// requeue puts the run back in the queue: its sandbox was not ready in time. It is the run's
// owner's last write; the next claim gives the run a new epoch.
func (r *runner) requeue(ctx context.Context, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcSeq++
	src := r.srcSeq
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		payload, _ := json.Marshal(map[string]string{"reason": reason})
		if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch,
			Kind: "run.requeued", Payload: payload, SourceSeq: src,
		}); err != nil {
			return err
		}
		_, err := q.RequeueRun(ctx, store.RequeueRunParams{OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errFenced // taken over, canceled or finished meanwhile
	}
	if err != nil {
		return r.dbErr(ctx, "requeue the run", err)
	}
	r.p.cfg.Metrics.Requeued.Inc()
	r.log.Info("run back in the queue", "reason", reason)
	return nil
}

// failSandboxUnavailable fails a run whose sandbox never became ready within the org's cap, in one
// transaction: the run's end, the agent's unhealthy mark with its audit event, the notification of
// the org's owners and admins, and a note on every task with a run waiting on the agent (ADR 0066
// amendment 2026-10-28). The agent's other queued runs stop being claimable at the same moment
// (ClaimNextRun skips unhealthy agents), so their tasks must say why they stopped moving.
func (r *runner) failSandboxUnavailable(ctx context.Context, f *failure) error {
	su := f.sandbox
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcSeq++
	src := r.srcSeq
	capd := su.cap
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if err := r.finishIn(ctx, q, src, store.RunStateFailed, f.reason, classSandboxUnavailable, map[string]any{
			"sandboxCause": string(su.cause), "waitedSeconds": int64(su.waited.Seconds()), "detail": su.detail,
		}); err != nil {
			return err
		}
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("its sandbox never became ready in %s: %s (%s)", runreply.Human(capd), su.cause.Describe(), su.detail)
		if _, err := q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{
			OrgID: r.run.OrgID, ID: r.run.AgentID, Reason: reason,
		}); errors.Is(err, pgx.ErrNoRows) {
			return nil // already unhealthy: it was told, audited and noted
		} else if err != nil {
			return err
		}
		if err := audit.RecordErr(ctx, q, audit.Event{
			Org: r.run.OrgID, Actor: audit.System(), Action: "agent.mark_unhealthy",
			TargetKind: "agent", TargetID: r.run.AgentID.String(),
			After: map[string]any{
				"reason": reason, "cause": "sandbox_unavailable", "sandboxCause": string(su.cause),
				"runId": r.run.ID.String(), "waitedSeconds": int64(su.waited.Seconds()),
			},
		}); err != nil {
			return err
		}
		// The decision says "the agent's owners". Koinon has no per-agent owner (agents.created_by is
		// only who created it): the people who can act are the ones who can resume an agent, the
		// org's admins and owners.
		message := fmt.Sprintf("%s was marked unhealthy: its sandbox never became ready in %s (%s). Fix the cause, then resume the agent on its page.",
			a.DisplayName, runreply.Human(capd), su.cause.Describe())
		if err := r.notifyUnhealthy(ctx, q, "agent.unhealthy:"+r.run.AgentID.String()+":sandbox-wait:"+r.run.ID.String(), map[string]string{
			"run_id": r.run.ID.String(), "sandbox_cause": string(su.cause), "message": message,
		}); err != nil {
			return err
		}
		waiting, err := q.ListTasksWithWaitingRunOfAgent(ctx, store.ListTasksWithWaitingRunOfAgentParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		if err != nil {
			return err
		}
		tasks := map[pgtype.UUID]bool{r.run.TaskID: true}
		for _, t := range waiting {
			tasks[t] = true
		}
		for t := range tasks {
			if err := r.taskNote(ctx, q, t, kindAgentUnhealthy,
				message+" Its waiting runs do not start until an admin resumes the agent."); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return r.dbErr(ctx, "fail the run", err)
	}
	r.p.cfg.Metrics.Finished.WithLabelValues(string(store.RunStateFailed)).Inc()
	r.p.cfg.Metrics.SandboxWaitCap.WithLabelValues(string(su.cause)).Inc()
	r.log.Error("the sandbox never became ready", "cause", su.cause, "detail", su.detail, "waited", su.waited)
	return nil
}
