// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// The crash and liveness path (issue #1481). A driver that can revert (execution.Reverter) gets it:
//
//   - A CRASHED sandbox is reverted to its last snapshot and resumed, which keeps the home. Only when
//     the revert is not possible (no snapshot) or fails, or a complete home checkpoint is newer than
//     any write to the home, the sandbox goes through the delete and the restore of repair.
//   - A ready sandbox whose guest front is silent for frontSilence is forced to crash, so the first
//     rule applies on the next pass. Only sandboxes in phase Ready are checked, and the check never
//     goes through the router ingress: it would resume a suspended sandbox.
//   - An agent is reverted at most maxReverts times in revertWindow. After that the agent is marked
//     unhealthy with the last failure as the reason, so a front that always fails does not loop.
const (
	maxReverts   = 3
	revertWindow = time.Hour
	// frontSilence is how long the front may stay silent before the sandbox is forced to crash.
	frontSilence = 30 * time.Second
)

// crashTracker holds the counters of the crash path in memory: they restart with the process, which
// only gives an agent more reverts and a silent front a fresh silence window.
type crashTracker struct {
	mu      sync.Mutex
	reverts map[pgtype.UUID][]time.Time
	silent  map[pgtype.UUID]time.Time
}

// noteRevert records a revert and returns how many reverts of the agent are inside the window.
func (t *crashTracker) noteRevert(id pgtype.UUID, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reverts == nil {
		t.reverts = map[pgtype.UUID][]time.Time{}
	}
	t.reverts[id] = append(t.recent(id, now), now)
	return len(t.reverts[id])
}

// count returns how many reverts of the agent are inside the window.
func (t *crashTracker) count(id pgtype.UUID, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.recent(id, now))
}

// recent returns the reverts inside the window. The caller holds t.mu.
func (t *crashTracker) recent(id pgtype.UUID, now time.Time) []time.Time {
	var keep []time.Time
	for _, at := range t.reverts[id] {
		if now.Sub(at) < revertWindow {
			keep = append(keep, at)
		}
	}
	return keep
}

// silentSince notes that the front did not answer and returns when its silence began.
func (t *crashTracker) silentSince(id pgtype.UUID, now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.silent == nil {
		t.silent = map[pgtype.UUID]time.Time{}
	}
	if at, ok := t.silent[id]; ok {
		return at
	}
	t.silent[id] = now
	return now
}

func (t *crashTracker) answered(id pgtype.UUID) {
	t.mu.Lock()
	delete(t.silent, id)
	t.mu.Unlock()
}

// checkFront is the liveness check of a ready sandbox. It returns true when it forced the sandbox to
// crash: the pass then ends, and the next one reverts it.
func (r *Reconciler) checkFront(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string) (bool, error) {
	rv, ok := execution.ReverterOf(r.backend)
	if !ok || sb.Phase != execution.PhaseReady {
		return false, nil
	}
	now := r.cfg.Now()
	err := rv.CheckFront(ctx, ws, name)
	switch {
	case err == nil:
		r.crash.answered(w.agent.ID)
		return false, nil
	case !errors.Is(err, execution.ErrFrontDown):
		// Not a verdict on the front (the router is down, the sandbox is not running any more).
		return false, nil
	}
	since := r.crash.silentSince(w.agent.ID, now)
	if now.Sub(since) < frontSilence {
		return false, nil
	}
	r.log.Warn("the guest front does not answer; forcing the sandbox to crash so it is reverted",
		append(w.logAttrs(), "silent_for", now.Sub(since).String())...)
	if err := rv.FaultSandbox(ctx, ws, name); err != nil {
		return false, fmt.Errorf("reconciler: fault a sandbox with a silent front: %w", err)
	}
	r.crash.answered(w.agent.ID)
	return true, r.record(ctx, w, sb, hash, "the guest front did not answer; reverting the sandbox to its last snapshot")
}

// revertCrashed reverts a crashed sandbox. handled is false when the caller must run the repair path
// (the delete and the restore from the checkpoint).
func (r *Reconciler) revertCrashed(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string) (handled bool, err error) {
	rv, ok := execution.ReverterOf(r.backend)
	if !ok || sb.Phase != execution.PhaseError || w.agent.UnhealthyAt.Valid {
		return false, nil
	}
	since := homeDirtiedSince(w.sb)
	// A complete checkpoint that is newer than every write to the home holds all of the home. The
	// snapshot of Substrate cannot be newer than the writes it lacks, so the checkpoint is used.
	if cp, err := r.q.LatestHomeCheckpointOfSandbox(ctx, store.LatestHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID}); err == nil {
		if !since.IsZero() && fresh(cp, since) {
			return false, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read the latest home checkpoint: %w", err)
	}
	now := r.cfg.Now()
	detail := "phase " + string(sb.Phase)
	if m := truncate(sb.Message, 300); m != "" {
		detail += ": " + m
	}
	if n := r.crash.count(w.agent.ID, now); n >= maxReverts {
		reason := fmt.Sprintf("the front kept failing: %d reverts in %s; the last failure: %s", n, revertWindow, detail)
		if _, err := r.q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: w.org.ID, ID: w.agent.ID, Reason: reason}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return true, r.record(ctx, w, sb, hash, "")
			}
			return true, err
		}
		r.m.AgentsUnhealthy.Inc()
		r.log.Error("the sandbox keeps crashing after a revert; the agent is marked unhealthy", append(w.logAttrs(), "reverts", n, "last", detail)...)
		if err := audit.RecordAfter(ctx, r.db, audit.Event{
			Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-revert"), Action: "agent.mark_unhealthy", TargetKind: "agent", TargetID: w.agent.ID.String(),
			After: map[string]any{"sandbox": name, "reason": reason, "reverts": n},
		}); err != nil {
			return true, err
		}
		return true, r.record(ctx, w, sb, hash, "")
	}
	res, err := rv.RevertSandbox(ctx, ws, name)
	switch {
	case errors.Is(err, execution.ErrNoSnapshot):
		// The actor never suspended: a revert would give an empty home. Repair recreates it.
		return false, nil
	case errors.Is(err, execution.ErrUnavailable):
		return true, err // a worker has no room now (after the backoff of the driver): try on the next pass
	case err != nil:
		r.log.Warn("the revert failed; the sandbox goes through the repair path", append(w.logAttrs(), "err", err)...)
		return false, nil
	}
	r.crash.noteRevert(w.agent.ID, now)
	lost := "the writes after the last snapshot (its time is unknown)"
	after := map[string]any{"sandbox": name, "crash": truncate(sb.Message, 300)}
	if !res.SnapshotAt.IsZero() {
		lost = "the writes after " + res.SnapshotAt.UTC().Format(time.RFC3339)
		after["snapshot_at"] = res.SnapshotAt.UTC().Format(time.RFC3339)
	}
	r.log.Warn("reverted a crashed sandbox to its last snapshot", append(w.logAttrs(), "lost", lost, "detail", detail)...)
	if err := audit.RecordAfter(ctx, r.db, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-revert"), Action: "sandbox.revert", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: after,
	}); err != nil {
		return true, err
	}
	return true, r.record(ctx, w, res.Sandbox, hash, "reverted a crashed sandbox to its last snapshot; lost: "+lost)
}
