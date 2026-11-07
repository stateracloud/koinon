// SPDX-License-Identifier: Apache-2.0

package agentd

// A sandbox with an unkillable process (plan item AG1; ADR 0054).
//
// The sweep (sweep.go) ends what an earlier run left in the sandbox. When processes are still
// there after its last SIGKILL round, agentd watches them: every StuckPoll it reads their state
// from /proc (procinfo.sh) and, if any is still present StuckAfter (60 s) after the sweep gave up,
// whatever its state, the sandbox is stuck. A process that only awaits its parent's wait
// (zombie) is dead and does not count, as in the sweep.
//
// A stuck sandbox is handled in this order:
//
//  1. the workspace's uncommitted work is read and sealed (savework.go), within SaveTimeout;
//  2. one transaction records the incident with what /proc said (pid, comm, state, wchan), and
//     - under the loop guard of three recreations per agent in 24 hours - marks the sandbox
//     unhealthy, so koinon-reconciler recreates it with a new home (the reset path of ADR 0051
//     then starts new sessions, parked runs included), and puts a note on the task; or marks the
//     agent unhealthy, so new runs are refused until an admin resumes it, and tells the org's
//     owners and admins;
//  3. the reconciler is woken.
//
// The marking is audited. Nothing here is fatal to a run that ended well.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// Stuck-sandbox defaults and limits.
const (
	defaultStuckAfter  = 60 * time.Second
	defaultStuckPoll   = 5 * time.Second
	defaultSaveTimeout = 30 * time.Second
	// MaxRecreations is the loop guard: recreations per agent per 24 hours.
	MaxRecreations = 3
)

// Task event kinds of a stuck sandbox.
const (
	kindSandboxRecreated = "sandbox.recreated"
	kindAgentUnhealthy   = runoutcome.KindAgentUnhealthy
	kindWorkLost         = "sandbox.work_lost"
)

// NotificationAgentUnhealthy is the in-app notification kind for owners and admins.
const NotificationAgentUnhealthy = runoutcome.NotificationAgentUnhealthy

type survivor struct{ pid, start string }

func (s survivor) String() string { return s.pid + ":" + s.start }

// survivorsError: processes outlived the sweep's last SIGKILL round.
type survivorsError struct {
	survivors []survivor
	detail    string
}

func (e *survivorsError) Error() string {
	pids := make([]string, len(e.survivors))
	for i, s := range e.survivors {
		pids[i] = s.pid
	}
	return "end the processes left in the sandbox: still running: " + strings.Join(pids, " ")
}

// parseSurvivors reads the sweep's "survivors: PID:START ..." line.
func parseSurvivors(out string) []survivor {
	var res []survivor
	for line := range strings.Lines(out) {
		v, ok := strings.CutPrefix(line, "survivors:")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(v) {
			pid, start, ok := strings.Cut(f, ":")
			if _, err := strconv.Atoi(pid); ok && err == nil && start != "" {
				res = append(res, survivor{pid, start})
			}
		}
	}
	return res
}

// stuckProc is what /proc said about a process that would not go.
type stuckProc struct {
	PID   int    `json:"pid"`
	State string `json:"state"`
	Comm  string `json:"comm"`
	Wchan string `json:"wchan"`
}

// parseProcinfo reads procinfo.sh's "PID|STATE|COMM|WCHAN" lines.
func parseProcinfo(out string) []stuckProc {
	var res []stuckProc
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimRight(line, "\r\n"), "|", 4)
		if len(f) != 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		res = append(res, stuckProc{PID: pid, State: f[1], Comm: f[2], Wchan: f[3]})
	}
	return res
}

// stuckOutcome is what handleSurvivors decided.
type stuckOutcome int

const (
	stuckGone      stuckOutcome = iota + 1 // the processes ended on their own
	stuckUnknown                           // the sandbox could not be read: nothing was decided
	stuckRecreate                          // the sandbox is marked unhealthy; the reconciler recreates it
	stuckUnhealthy                         // the loop guard stopped the agent
	stuckFenced                            // the worker may no longer act
)

// watch re-reads the survivors until they are gone or StuckAfter has passed, and returns the ones
// still there.
func (r *runner) watch(ctx context.Context, workspace, sandbox string, sv []survivor) (procs []stuckProc, read bool, fenced bool) {
	deadline := time.Now().Add(r.p.cfg.StuckAfter)
	args := []string{"/bin/sh", "-c", procinfoScript, "procinfo"}
	for _, s := range sv {
		args = append(args, s.String())
	}
	for {
		out, code, stderr, err := r.sandboxExecLimit(ctx, workspace, sandbox, args, nil, 20*time.Second, 64<<10)
		switch {
		case err != nil || code != 0:
			r.log.Warn("reading the processes the sweep could not end failed", "err", err, "exit", code, "stderr", stderr)
		default:
			read = true
			procs = parseProcinfo(string(out))
			if len(procs) == 0 {
				return nil, true, false
			}
		}
		if !time.Now().Before(deadline) {
			return procs, read, false
		}
		select {
		case <-r.own.Fenced():
			return nil, read, true
		default:
		}
		if !sleep(ctx, min(r.p.cfg.StuckPoll, time.Until(deadline))) {
			return nil, read, true
		}
	}
}

// handleSurvivors watches the processes the sweep could not end and, if they stay, handles the
// stuck sandbox.
func (r *runner) handleSurvivors(ctx context.Context, workspace, sandbox string, sv *survivorsError) (stuckOutcome, error) {
	ctx = context.WithoutCancel(ctx)
	procs, read, fenced := r.watch(ctx, workspace, sandbox, sv.survivors)
	switch {
	case fenced:
		return stuckFenced, nil
	case len(procs) == 0 && read:
		return stuckGone, nil
	case len(procs) == 0:
		return stuckUnknown, nil
	}
	r.log.Warn("the sandbox is stuck: processes survive SIGKILL", "processes", procs)
	return r.recreateStuck(ctx, workspace, sandbox, procs)
}

// stuckAfterTurn handles survivors of the end-of-turn sweep. The run is not affected.
func (r *runner) stuckAfterTurn(ctx context.Context, workspace, sandbox string, sv *survivorsError) {
	out, err := r.handleSurvivors(ctx, workspace, sandbox, sv)
	if err != nil {
		r.log.Error("handling the stuck sandbox failed", "err", err)
		return
	}
	r.log.Info("handled processes that survived the sweep", "outcome", out)
}

// reason is the stuck sandbox in one sentence, for the timeline and the audit log.
func stuckReason(procs []stuckProc, after time.Duration) string {
	parts := make([]string, 0, len(procs))
	for _, p := range procs {
		parts = append(parts, fmt.Sprintf("%s (pid %d, state %s, waiting in %s)", p.Comm, p.PID, p.State, cmpWchan(p.Wchan)))
	}
	return fmt.Sprintf("a process could not be ended for %s: %s", after.Round(time.Second), strings.Join(parts, "; "))
}

func cmpWchan(w string) string {
	if w == "" || w == "0" {
		return "an unknown place"
	}
	return w
}

// recreateStuck saves the workspace's work, records the incident and marks the sandbox, or the
// agent when the loop guard says so.
func (r *runner) recreateStuck(ctx context.Context, workspace, sandbox string, procs []stuckProc) (stuckOutcome, error) {
	saved := r.saveWork(ctx, workspace, sandbox)
	reason := stuckReason(procs, r.p.cfg.StuckAfter)
	procJSON, err := json.Marshal(procs)
	if err != nil {
		return 0, err
	}
	outcome := stuckRecreate
	var incidentID pgtype.UUID
	err = pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		n, err := q.CountRecentSandboxRecreations(ctx, store.CountRecentSandboxRecreationsParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		if err != nil {
			return err
		}
		outcome = stuckRecreate
		kind := "recreate"
		if n >= MaxRecreations {
			outcome, kind = stuckUnhealthy, "agent_unhealthy"
		}
		work, note := saved.status, saved.note()
		var firstSaved pgtype.UUID
		for _, w := range saved.rows {
			if !firstSaved.Valid {
				firstSaved = w.id
			}
		}
		inc, err := q.InsertSandboxIncident(ctx, store.InsertSandboxIncidentParams{
			OrgID: r.run.OrgID, AgentID: r.run.AgentID, RunID: r.run.ID, TaskID: r.run.TaskID,
			Processes: procJSON, Outcome: kind, Work: work, WorkNote: note, SavedWorkID: firstSaved,
		})
		if err != nil {
			return err
		}
		incidentID = inc.ID
		ev := audit.Event{
			Org: r.run.OrgID, Actor: audit.System("koinon-agentd:stuck-run"), TargetKind: "agent", TargetID: r.run.AgentID.String(),
			After: map[string]any{
				"sandbox": sandbox, "reason": reason, "processes": procs, "work": work, "incident": inc.ID.String(),
				"recreations_in_24h": n,
			},
		}
		if outcome == stuckUnhealthy {
			ev.Action = "agent.mark_unhealthy"
			if _, err := q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{
				OrgID: r.run.OrgID, ID: r.run.AgentID,
				Reason: fmt.Sprintf("%d sandbox recreations in 24 hours; the last: %s", n, reason),
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err := r.notifyUnhealthy(ctx, q, "agent.unhealthy:"+r.run.AgentID.String()+":"+inc.ID.String(), map[string]string{
				"incident_id": inc.ID.String(),
				"message":     "An agent is unhealthy and will not start runs until an admin resumes it. " + reason,
			}); err != nil {
				return err
			}
			waiting, err := q.ListTasksWithWaitingRunOfAgent(ctx, store.ListTasksWithWaitingRunOfAgentParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
			if err != nil {
				return err
			}
			notify := map[pgtype.UUID]bool{r.run.TaskID: true}
			for _, t := range waiting {
				notify[t] = true
			}
			for t := range notify {
				if err := r.taskNote(ctx, q, t, kindAgentUnhealthy,
					"The agent is unhealthy: its sandbox got stuck again ("+strconv.FormatInt(n, 10)+
						" recreations in 24 hours), so it is not recreated. New runs are refused, and this task's waiting run does not start until an admin resumes the agent. "+reason); err != nil {
					return err
				}
			}
		} else {
			ev.Action = "sandbox.mark_unhealthy"
			if _, err := q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{
				OrgID: r.run.OrgID, AgentID: r.run.AgentID, Reason: reason, Generation: r.sandboxID,
			}); err != nil {
				return err
			}
			tasks, err := q.ListTasksWithParkedRunOfAgent(ctx, store.ListTasksWithParkedRunOfAgentParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
			if err != nil {
				return err
			}
			notify := map[pgtype.UUID]bool{r.run.TaskID: true}
			for _, t := range tasks {
				notify[t] = true
			}
			for t := range notify {
				if err := r.taskNote(ctx, q, t, kindSandboxRecreated, "Sandbox recreated: "+reason); err != nil {
					return err
				}
			}
		}
		if work == "lost" {
			if err := r.taskNote(ctx, q, r.run.TaskID, kindWorkLost,
				"The agent's uncommitted work was lost. Details (the sandbox reported some of them; they are not verified): "+note); err != nil {
				return err
			}
		}
		return audit.RecordErr(ctx, q, ev)
	})
	if err != nil {
		return 0, fmt.Errorf("record the stuck sandbox: %w", err)
	}
	if outcome == stuckRecreate {
		if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil {
			r.log.Warn("notifying the reconciler failed; it recreates the sandbox on its next pass", "err", err)
		}
	}
	r.log.Warn("stuck sandbox handled", "outcome", outcome, "incident", incidentID.String(), "work", saved.status)
	return outcome, nil
}

// taskNote appends a system event with a message to a task's timeline.
func (r *runner) taskNote(ctx context.Context, q *store.Queries, task pgtype.UUID, kind, message string) error {
	return runoutcome.TaskNote(ctx, q, r.run.OrgID, task, kind, message)
}

// notifyUnhealthy tells the org's owners and admins, once per dedupe key (internal/runoutcome).
func (r *runner) notifyUnhealthy(ctx context.Context, q *store.Queries, dedupe string, fields map[string]string) error {
	return runoutcome.NotifyUnhealthy(ctx, q, r.run, dedupe, fields)
}
