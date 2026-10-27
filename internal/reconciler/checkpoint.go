// SPDX-License-Identifier: Apache-2.0

package reconciler

// The home checkpoint gates (plan item HC1; ADR 0032; practice B3).
//
// koinon-agentd writes the checkpoints; the reconciler only asks for them and waits. A sandbox is
// deleted for a recreation (a changed spec, a stuck or failed sandbox) or an archive only after a
// checkpoint taken since its last activity is complete. The checkpoint then becomes the sandbox's
// restore (SetSandboxRestore): the next sandbox keeps the home generation, and the first run
// restores the home before the harness starts (agentd). A failed backup blocks the recreation and
// marks the agent unhealthy with the reason, until an admin resumes it. An idle stop is also
// preceded by a checkpoint, but a failed one only delays the stop (stopping keeps the volume).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// DefaultCheckpointStopWait is how long an idle stop waits for its checkpoint.
const DefaultCheckpointStopWait = 5 * time.Minute

// checkpointsOn reports whether the agent's home is checkpointed.
func (r *Reconciler) checkpointsOn(w *work) bool {
	return r.cfg.Checkpoints && !checkpoint.Ephemeral(w.agent)
}

// fresh reports whether a complete checkpoint was finished since the sandbox's last activity.
func fresh(cp store.HomeCheckpoint, since time.Time) bool {
	return cp.Status == store.CheckpointStatusComplete && cp.FinishedAt.Valid && !cp.FinishedAt.Time.Before(since)
}

// resumedSince reports whether an admin resumed the agent after the checkpoint failed.
func resumedSince(a store.Agent, cp store.HomeCheckpoint) bool {
	return a.ResumedAt.Valid && cp.FinishedAt.Valid && a.ResumedAt.Time.After(cp.FinishedAt.Time)
}

// requestCheckpoint asks for a checkpoint of the sandbox. An open request, whoever made it, is
// the one waited for.
func (r *Reconciler) requestCheckpoint(ctx context.Context, w *work, sb execution.Sandbox, reason store.CheckpointReason) error {
	_, err := r.q.RequestHomeCheckpoint(ctx, store.RequestHomeCheckpointParams{
		OrgID: w.org.ID, AgentID: w.agent.ID, Reason: reason, SandboxID: sb.ID, Generation: w.sb.HomeGeneration.String,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// homeGate decides whether the sandbox may be deleted (a recreation or an archive). It returns
// true when it may, after pointing the next sandbox's restore at the checkpoint; otherwise it
// returns the note to record while the recreation waits. since is the sandbox's last activity.
func (r *Reconciler) homeGate(ctx context.Context, w *work, ws string, sb execution.Sandbox, since time.Time) (proceed bool, note string, err error) {
	if !r.checkpointsOn(w) {
		return true, "", nil
	}
	if w.sb.RestoreCheckpointID.Valid {
		// The previous recreation's home has not been restored into this sandbox yet: it is
		// still in that checkpoint, and the sandbox holds nothing newer.
		return true, "", nil
	}
	if sb.Phase == execution.PhaseError || sb.Phase == execution.PhaseCompleted {
		return r.unreadableGate(ctx, w, sb, since)
	}
	cp, err := r.q.LatestHomeCheckpointOfSandbox(ctx, store.LatestHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID})
	hasCP := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("read the latest home checkpoint: %w", err)
	}
	switch {
	case hasCP && fresh(cp, since):
		if _, err := r.q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: w.org.ID, AgentID: w.agent.ID, CheckpointID: cp.ID}); err != nil {
			return false, "", fmt.Errorf("record the home restore: %w", err)
		}
		return true, "", nil
	case hasCP && (cp.Status == store.CheckpointStatusRequested || cp.Status == store.CheckpointStatusRunning):
		// Waiting. A stopped sandbox is started so that agentd can read it.
		return false, "waiting for the home backup before recreating the sandbox", r.wakeForBackup(ctx, w, ws, sb)
	case hasCP && cp.Status == store.CheckpointStatusFailed && cp.Reason == store.CheckpointReasonRecreate && !resumedSince(w.agent, cp):
		return false, "the home backup failed, so the sandbox is not recreated: " + cp.Error.String, r.blockRecreate(ctx, w, sb, cp)
	}
	if err := r.requestCheckpoint(ctx, w, sb, store.CheckpointReasonRecreate); err != nil {
		return false, "", fmt.Errorf("request a home checkpoint: %w", err)
	}
	r.log.Info("requested a home checkpoint before recreating the sandbox", w.logAttrs()...)
	return false, "waiting for the home backup before recreating the sandbox", r.wakeForBackup(ctx, w, ws, sb)
}

// unreadableGate is the gate for a sandbox in phase Error or Completed. agentd reads a home only
// from a ready sandbox, so a request filed now would never be claimed; instead the newest complete
// checkpoint of the sandbox (hourly, stop or recreate) is used when it was taken since the
// sandbox's last activity, and otherwise the recreation is blocked at once: the home is kept and
// the agent is marked unhealthy with the reason. Stopgap until the fallback pod that reads the
// volume directly exists (ADR 0032).
func (r *Reconciler) unreadableGate(ctx context.Context, w *work, sb execution.Sandbox, since time.Time) (bool, string, error) {
	cp, err := r.q.LatestCompleteHomeCheckpointOfSandbox(ctx, store.LatestCompleteHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID})
	hasCP := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("read the latest complete home checkpoint: %w", err)
	}
	if hasCP && fresh(cp, since) {
		if _, err := r.q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: w.org.ID, AgentID: w.agent.ID, CheckpointID: cp.ID}); err != nil {
			return false, "", fmt.Errorf("record the home restore: %w", err)
		}
		return true, "", nil
	}
	reason := fmt.Sprintf("the sandbox is in phase %s, so its home cannot be read and has no backup since its last activity; it is not recreated and its home is kept", sb.Phase)
	if hasCP {
		reason += fmt.Sprintf(" (the last backup is from %s)", cp.FinishedAt.Time.UTC().Format(time.RFC3339))
	}
	return false, reason, r.markBlocked(ctx, w, sb, reason, cp.ID.String())
}

// wakeForBackup starts a stopped sandbox: agentd reads the home over exec. It stops again once
// idle, with its checkpoint complete.
func (r *Reconciler) wakeForBackup(ctx context.Context, w *work, ws string, sb execution.Sandbox) error {
	if sb.Phase != execution.PhaseStopped {
		return nil
	}
	if _, err := r.backend.StartSandbox(ctx, ws, w.names().Sandbox); err != nil {
		return err
	}
	r.log.Info("started the sandbox to back up its home", w.logAttrs()...)
	return nil
}

// blockRecreate marks the agent unhealthy with the failed backup as the reason, once. An admin
// resumes it (ResumeAgent), which makes the next pass request a new backup.
func (r *Reconciler) blockRecreate(ctx context.Context, w *work, sb execution.Sandbox, cp store.HomeCheckpoint) error {
	return r.markBlocked(ctx, w, sb, "the home backup failed, so the sandbox is not recreated and its home is kept: "+cp.Error.String, cp.ID.String())
}

// markBlocked marks the agent unhealthy with reason, once, and audits it.
func (r *Reconciler) markBlocked(ctx context.Context, w *work, sb execution.Sandbox, reason, checkpointID string) error {
	if w.agent.UnhealthyAt.Valid {
		return nil
	}
	if _, err := r.q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: w.org.ID, ID: w.agent.ID, Reason: reason}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	r.m.AgentsUnhealthy.Inc()
	r.log.Error("the sandbox is not recreated without a backup of its home", append(w.logAttrs(), "reason", reason)...)
	return audit.RecordErr(ctx, r.q, audit.Event{
		Org: w.org.ID, Actor: audit.System(), Action: "agent.mark_unhealthy", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: map[string]any{"sandbox": w.names().Sandbox, "reason": reason, "checkpoint": checkpointID},
	})
}

// stopGate returns true when the idle sandbox may be stopped: its home was checkpointed since its
// last activity, or the checkpoint failed or is taking longer than CheckpointStopWait (a stop
// keeps the volume, so it does not wait for a backup that will not come).
func (r *Reconciler) stopGate(ctx context.Context, w *work, sb execution.Sandbox, since time.Time) (bool, error) {
	if !r.checkpointsOn(w) || w.sb.RestoreCheckpointID.Valid {
		return true, nil
	}
	cp, err := r.q.LatestHomeCheckpointOfSandbox(ctx, store.LatestHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID})
	hasCP := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	wait := r.cfg.CheckpointStopWait
	if wait == 0 {
		wait = DefaultCheckpointStopWait
	}
	switch {
	case hasCP && fresh(cp, since):
		return true, nil
	case hasCP && cp.Status == store.CheckpointStatusFailed && cp.FinishedAt.Valid && !cp.FinishedAt.Time.Before(since):
		r.log.Warn("stopping an idle sandbox although its last home backup failed", append(w.logAttrs(), "err", cp.Error.String)...)
		return true, nil
	case hasCP && (cp.Status == store.CheckpointStatusRequested || cp.Status == store.CheckpointStatusRunning):
		return r.cfg.Now().Sub(cp.RequestedAt.Time) >= wait, nil
	}
	if err := r.requestCheckpoint(ctx, w, sb, store.CheckpointReasonStop); err != nil {
		return false, err
	}
	return false, nil
}
