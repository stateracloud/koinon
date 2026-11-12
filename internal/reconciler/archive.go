// SPDX-License-Identifier: Apache-2.0

package reconciler

// The archive tier (plan item AR1; ADR 0032 amendment, old issue 146 A7; practice B3).
//
// A sandbox that has been stopped for ArchiveAfter is deleted together with its volume, but only
// when its home's latest checkpoint is complete and was taken since the sandbox's last activity
// (the same freshness rule as homeGate: since the home was last written; a sandbox no harness ran
// in is archived without a new backup). That checkpoint becomes the sandbox's restore, so the
// next sandbox keeps the home generation and the first run restores the home before the harness
// starts (HC1, unchanged); agentd then reloads the same harness session (session/load).
//
// Unlike a recreation, the archive never wakes a sandbox, files a request or marks the agent
// unhealthy: a stopped sandbox without a fresh complete checkpoint (failed, still running, older
// than its last activity) is simply not archived, and its volume is kept.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// DefaultArchiveAfter is how long a sandbox stays stopped before it is archived.
const DefaultArchiveAfter = 7 * 24 * time.Hour

const (
	archivedPhase   = "Archived"
	archivedMessage = "archived, restores on next run"
	// idlePhase is recorded for an active agent whose sandbox is not created yet (old issue 315).
	idlePhase   = "Idle"
	idleMessage = "no sandbox yet, it starts with the first run"
)

// archiveDue reports whether the stopped sandbox has been stopped long enough to archive.
func (r *Reconciler) archiveDue(w *work, since time.Time) bool {
	if r.cfg.ArchiveAfter < 0 || !r.checkpointsOn(w) || w.sb.State != store.SandboxStateStopped || !w.sb.StoppedAt.Valid {
		return false
	}
	from := w.sb.StoppedAt.Time
	if since.After(from) {
		from = since
	}
	return !r.cfg.Now().Before(from.Add(r.cfg.ArchiveAfter))
}

// archive deletes a long-stopped sandbox and its volume when its home is safely checkpointed. It
// reports done when it did, or when the pass has recorded what it needed to.
//
// A parked run does not prevent it: a parked run resumes its session in the home, and the restore
// keeps the generation, so the session is reloaded in the fresh sandbox (ADR 0051). Active and
// claimable runs keep the sandbox up (the caller checks).
func (r *Reconciler) archive(ctx context.Context, w *work, ws string, sb execution.Sandbox, act store.GetAgentRunActivityRow, hash string) (bool, error) {
	since := activitySince(w.sb, act)
	if !r.archiveDue(w, since) {
		return false, nil
	}
	if dirtied := homeDirtiedSince(w.sb); !w.sb.RestoreCheckpointID.Valid && dirtied.IsZero() {
		// No harness ran in this sandbox: its home is what the generation's newest checkpoint holds.
		if err := r.restoreUntouched(ctx, w); err != nil {
			return false, err
		}
	} else if !w.sb.RestoreCheckpointID.Valid {
		// Otherwise the sandbox holds nothing newer than the checkpoint it was restored from.
		cp, err := r.q.LatestHomeCheckpointOfSandbox(ctx, store.LatestHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !fresh(cp, dirtied) {
			r.log.Debug("not archiving a stopped sandbox: its latest home backup is not complete", w.logAttrs()...)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read the latest home checkpoint: %w", err)
		}
		if _, err := r.q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: w.org.ID, AgentID: w.agent.ID, CheckpointID: cp.ID}); err != nil {
			return false, fmt.Errorf("record the home restore: %w", err)
		}
	}
	if err := r.deleteSandbox(ctx, w, ws, w.names().Sandbox, "archive of a stopped sandbox"); err != nil {
		return false, err
	}
	r.log.Info("archived a stopped sandbox and its volume", w.logAttrs()...)
	if err := audit.RecordAfter(ctx, r.pool, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-archive"), Action: "sandbox.archive", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: map[string]any{"sandbox": w.names().Sandbox, "stopped_for": r.cfg.Now().Sub(w.sb.StoppedAt.Time).Round(time.Second).String(), "home_generation": w.sb.HomeGeneration.String},
	}); err != nil {
		return false, err
	}
	sb.Phase = execution.PhaseDeleting
	if err := r.record(ctx, w, sb, hash, archivedMessage); err != nil {
		return false, err
	}
	row, err := r.q.MarkSandboxArchived(ctx, store.MarkSandboxArchivedParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	if err != nil {
		return false, fmt.Errorf("record the archive: %w", err)
	}
	w.sb = row
	return true, nil
}
