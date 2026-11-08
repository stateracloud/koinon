// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// ephemeralTurn gives the agent's row the sandbox it should have now and reports whether the
// pass has nothing more to do for the sandbox (ADR 0065 decision 13c).
//
// A persistent agent has one sandbox, named from the agent, for good. An ephemeral agent has one
// sandbox per task, named from the agent and the task (execution.TaskSandboxName), with the
// workspace volume OpenShell 0.1.5 gives every sandbox and deletes with it (see sandboxSpec). The
// agent keeps its single agent_sandboxes row, which points at the sandbox of the task that needs
// one: the task of the run that holds the agent, else of the run claimed next, else of a parked
// run, else of the latest run of a task that is still open (GetEphemeralFocusTask). When that task
// changes, the previous sandbox is deleted, volume and all, and the row moves to the new name
// once it is gone. A task between runs keeps its sandbox (the idle stop still applies); only the
// task's end (completed, canceled, failed) deletes it, and it is not recreated. Nothing carries over
// between tasks: memory is the task's notes and the ADR 0033 stores.
//
// A persistent agent whose row still names a task sandbox (the version that made it ephemeral is
// no longer the active one) is moved back to the agent's own sandbox the same way. A running run
// is never cut off: the row is left alone until it ends.
func (r *Reconciler) ephemeralTurn(ctx context.Context, w *work, ws string, act store.GetAgentRunActivityRow) (done bool, err error) {
	ephemeral := agentconfig.IsEphemeral(w.cfg)
	persistent := execution.AgentSandboxName(w.org.ID.Bytes, w.agent.ID.Bytes)
	if !ephemeral && w.sb.SandboxName == persistent {
		return false, nil
	}
	want := persistent
	focused := true
	if ephemeral {
		task, err := r.q.GetEphemeralFocusTask(ctx, store.GetEphemeralFocusTaskParams{OrgID: w.org.ID, AgentID: w.agent.ID})
		switch {
		case err == nil:
			want = execution.TaskSandboxName(w.org.ID.Bytes, w.agent.ID.Bytes, task.Bytes)
		case errors.Is(err, pgx.ErrNoRows):
			focused, want = false, w.sb.SandboxName
		default:
			return false, fmt.Errorf("read the ephemeral agent's task: %w", err)
		}
	}
	if act.HasRunning && w.sb.SandboxName != want {
		// A run that is running still uses the current sandbox; it ends first.
		return false, nil
	}

	cur := w.sb.SandboxName
	sb, err := r.backend.GetSandbox(ctx, ws, cur)
	gone := errors.Is(err, execution.ErrNotFound)
	if err != nil && !gone {
		return false, err
	}
	if cur == want && focused {
		return false, nil
	}
	if !gone {
		// The previous task's sandbox, or one left with no task to serve: delete it with its volume.
		if sb.Phase != execution.PhaseDeleting {
			if cur == persistent {
				// The agent became ephemeral: its persistent home is deleted only after a fresh
				// checkpoint, as for any recreation (ADR 0032). The restore the gate records is
				// cleared when the row moves to the task sandbox (SetAgentSandboxName): nothing
				// carries over into a task sandbox, the checkpoint is kept for a later persistent version.
				pw := *w
				pw.cfg.Ephemeral = false
				ok, note, err := r.homeGate(ctx, &pw, ws, sb, homeDirtiedSince(w.sb))
				if err != nil {
					return false, err
				}
				if !ok {
					return true, r.record(ctx, w, sb, w.sb.SpecHash, note)
				}
			}
			if err := r.backend.DeleteSandbox(ctx, ws, cur); err != nil {
				return false, err
			}
			r.log.Info("deleted the sandbox of an ended task", w.logAttrs()...)
			// Not "sandbox.recreate" and no phase: ADR 0054's loop guard counts those.
			if err := audit.RecordAfter(ctx, r.q, audit.Event{
				Org: w.org.ID, Actor: audit.System("koinon-reconciler:ephemeral-cleanup"), Action: "sandbox.delete", TargetKind: "agent", TargetID: w.agent.ID.String(),
				After: map[string]any{"sandbox": cur, "reason": "ephemeral task ended"},
			}); err != nil {
				return false, err
			}
		}
		sb.Phase = execution.PhaseDeleting
		return true, r.record(ctx, w, sb, w.sb.SpecHash, "deleting the sandbox of the previous task")
	}
	if cur != want {
		// Gone: the row moves on, and the pass renders the new sandbox.
		row, err := r.q.SetAgentSandboxName(ctx, store.SetAgentSandboxNameParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxName: want})
		if err != nil {
			return false, fmt.Errorf("record the sandbox of the next task: %w", err)
		}
		w.sb = row
		return false, nil
	}
	// No task needs a sandbox and none exists: nothing to render.
	if w.sb.State != store.SandboxStatePending {
		return true, r.recordState(ctx, w, store.SandboxStatePending, "", "no task needs a sandbox", "")
	}
	return true, nil
}
