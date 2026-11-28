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
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The crash and liveness path (issue #1481). A driver that can revert (execution.Reverter) gets it.
//
// A CRASHED sandbox, and a RUNNING one whose guest front is dead, are recovered the same way. A dead
// front is reverted directly from RUNNING (RevertActor accepts it): a SuspendActor first would save the
// broken state over the last good snapshot.
//
//  1. The restored-home mark: when a restore ended and no snapshot holds the restored home yet, the
//     restore pointer is written again BEFORE the revert, in one statement that reads the row
//     (RestoreFromMark). The revert goes back to a snapshot that lacks the home; the pointer makes the
//     next run restore it. A write that fails stops the revert, so a mark is never left with no restore.
//  2. Snapshot or checkpoint: without a mark, the newer of the last snapshot and the newest complete home
//     checkpoint wins. All times are of the database clock: last_snapshot_at (read before the snapshot
//     call), the checkpoint's started_at, restored_at. An unknown snapshot time loses to a checkpoint. A
//     checkpoint that wins points the restore at it and the sandbox goes through the repair path (delete,
//     recreate, restore).
//  3. Otherwise RevertSandbox. ErrNoSnapshot (it never suspended) goes to repair. ErrRevertResume (the
//     revert worked, the egress write or the resume did not) keeps the sandbox: it holds its snapshot.
//
// The retry is bounded two ways. A revert that RevertActor accepted counts toward maxReverts in
// revertWindow, counted after the last admin resume. A revert that fails before RevertActor worked backs
// off, and after maxRevertFailures tries in a row the sandbox goes through the repair path.
//
// The front check needs frontProbes failed checks in a row over frontSilence, starts only frontGrace
// after the sandbox was first seen ready, and counts a deadline as silence only when the control API
// answered fast in the same call (the driver decides).
const (
	maxReverts   = 3
	revertWindow = time.Hour
	// frontSilence is how long the front stays silent, with frontProbes checks in a row, before it is
	// reverted.
	frontSilence = 30 * time.Second
	frontProbes  = 3
	// frontGrace is how long after a sandbox was first seen ready the front is left alone: a resume or a
	// revert has just ended and the front may still be starting.
	frontGrace = 30 * time.Second
	// maxRevertFailures is how many reverts in a row may fail before RevertActor worked, with a growing
	// wait, before the sandbox goes through the repair path.
	maxRevertFailures = 6
	revertBackoff     = 5 * time.Second
	revertBackoffMax  = 2 * time.Minute
)

// crashTracker holds the counters of the crash path in memory: they restart with the process, which
// only gives an agent more reverts and a silent front a fresh silence window. An admin resume resets the
// revert cap: only what happened after the agent's ResumedAt counts.
type crashTracker struct {
	mu         sync.Mutex
	reverts    map[pgtype.UUID][]time.Time
	silent     map[pgtype.UUID]silence
	readySince map[pgtype.UUID]time.Time
	failures   map[pgtype.UUID]failures
}

type silence struct {
	since  time.Time
	probes int
}

type failures struct {
	n          int
	last, next time.Time
}

// afterResume reports whether at is after the admin resume (no resume: always).
func afterResume(at time.Time, resumed pgtype.Timestamptz) bool {
	return !resumed.Valid || at.After(resumed.Time)
}

// noteRevert records a revert that RevertActor accepted and ends the failure streak.
func (t *crashTracker) noteRevert(id pgtype.UUID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reverts == nil {
		t.reverts = map[pgtype.UUID][]time.Time{}
	}
	t.reverts[id] = append(t.reverts[id], now)
	delete(t.failures, id)
}

// count returns how many reverts of the agent are inside the window and after the admin resume.
func (t *crashTracker) count(id pgtype.UUID, now time.Time, resumed pgtype.Timestamptz) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var keep []time.Time
	n := 0
	for _, at := range t.reverts[id] {
		if now.Sub(at) < revertWindow {
			keep = append(keep, at)
			if afterResume(at, resumed) {
				n++
			}
		}
	}
	if t.reverts != nil {
		t.reverts[id] = keep
	}
	return n
}

// noteFailure records a revert that failed before RevertActor worked and returns how many failed in a
// row (after the admin resume).
func (t *crashTracker) noteFailure(id pgtype.UUID, now time.Time, resumed pgtype.Timestamptz) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == nil {
		t.failures = map[pgtype.UUID]failures{}
	}
	f := t.failures[id]
	if !afterResume(f.last, resumed) {
		f = failures{}
	}
	f.n++
	f.last = now
	f.next = now.Add(min(revertBackoff<<(f.n-1), revertBackoffMax))
	t.failures[id] = f
	return f.n
}

// backingOff reports whether the next revert try is still waiting.
func (t *crashTracker) backingOff(id pgtype.UUID, now time.Time, resumed pgtype.Timestamptz) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.failures[id]
	return ok && afterResume(f.last, resumed) && now.Before(f.next)
}

// failed returns how many tries in a row failed (after the admin resume).
func (t *crashTracker) failed(id pgtype.UUID, resumed pgtype.Timestamptz) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f, ok := t.failures[id]; ok && afterResume(f.last, resumed) {
		return f.n
	}
	return 0
}

// quiet forgets the failure streak of the agent.
func (t *crashTracker) quiet(id pgtype.UUID) {
	t.mu.Lock()
	delete(t.failures, id)
	t.mu.Unlock()
}

// seenReady notes that the sandbox is ready and returns since when it has been seen so.
func (t *crashTracker) seenReady(id pgtype.UUID, now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readySince == nil {
		t.readySince = map[pgtype.UUID]time.Time{}
	}
	if at, ok := t.readySince[id]; ok {
		return at
	}
	t.readySince[id] = now
	return now
}

// notReady forgets the ready time and the silence: the next time the sandbox is ready it is a new
// start, with its grace.
func (t *crashTracker) notReady(id pgtype.UUID) {
	t.mu.Lock()
	delete(t.readySince, id)
	delete(t.silent, id)
	t.mu.Unlock()
}

// noteSilent counts a failed front check and returns the count and when the silence began.
func (t *crashTracker) noteSilent(id pgtype.UUID, now time.Time) (int, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.silent == nil {
		t.silent = map[pgtype.UUID]silence{}
	}
	s, ok := t.silent[id]
	if !ok {
		s.since = now
	}
	s.probes++
	t.silent[id] = s
	return s.probes, s.since
}

func (t *crashTracker) answered(id pgtype.UUID) {
	t.mu.Lock()
	delete(t.silent, id)
	t.mu.Unlock()
}

// crashCause says why the crash path runs.
type crashCause int

const (
	causeCrashed crashCause = iota // the actor is CRASHED (phase Error)
	causeFront                     // the actor is RUNNING and its guest front does not answer
)

// checkFront is the liveness check of a ready sandbox. It returns true when it handled the sandbox:
// the pass then ends.
func (r *Reconciler) checkFront(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string) (bool, error) {
	rv, ok := execution.ReverterOf(r.backend)
	if !ok {
		return false, nil
	}
	id := w.agent.ID
	if sb.Phase != execution.PhaseReady || w.agent.UnhealthyAt.Valid {
		r.crash.notReady(id) // an old silence must not count against the next run of the sandbox
		return false, nil
	}
	now := r.cfg.Now()
	if now.Sub(r.crash.seenReady(id, now)) < frontGrace {
		return false, nil
	}
	err := rv.CheckFront(ctx, ws, name)
	switch {
	case err == nil:
		r.crash.answered(id)
		return false, nil
	case !errors.Is(err, execution.ErrFrontDown):
		// Not a verdict on the front (the router is down, the control API is slow, the sandbox is not
		// running any more): the silence so far is not proof.
		r.crash.answered(id)
		return false, nil
	}
	probes, since := r.crash.noteSilent(id, now)
	if probes < frontProbes || now.Sub(since) < frontSilence {
		return false, nil
	}
	r.log.Warn("the guest front does not answer; reverting the sandbox to its last snapshot",
		append(w.logAttrs(), "silent_for", now.Sub(since).String(), "probes", probes)...)
	return r.recoverSandbox(ctx, w, ws, name, sb, hash, causeFront)
}

// restoredMark reports whether the sandbox got a home restore that no snapshot holds yet. The mark is in
// Postgres (agentd sets it when the restore ends), so a leader change keeps it. It counts only for the
// sandbox that got the restore: a recreate with no restore does not keep it.
func restoredMark(sb store.AgentSandbox) bool {
	return sb.RestoredCheckpointID.Valid && sb.RestoredSandboxID.Valid && sb.RestoredSandboxID.String == sb.SandboxID.String
}

// snapshotAfterRestore reports whether a snapshot of the sandbox is recorded that is newer than its
// restore, so it holds the restored home. Both times are on the database clock: restored_at is now() of
// agentd's write, last_snapshot_at the clock read before the snapshot call (snapshotClock), a lower bound.
func snapshotAfterRestore(sb store.AgentSandbox) bool {
	return sb.LastSnapshotAt.Valid && sb.RestoredAt.Valid && sb.LastSnapshotAt.Time.After(sb.RestoredAt.Time)
}

// sealRestored takes the SYNC suspend after a home restore (issue #1481, item 8). The restore ends in
// the guest (agentd clears RestoreCheckpointID and sets the mark), and agentd never calls the control
// plane, so the reconciler sees the mark and seals. The suspend is the stop of an idle sandbox: it waits
// until the sandbox is idle as for the idle stop, it never stops a kept-warm sandbox, and it waits for
// the home backup of stopGate. Until the seal, recoverSandbox points the restore at a checkpoint again.
//
// The mark ends only after a stop that this function sent, or when the recorded snapshot time is newer
// than the restore (a stop that another path sent and recorded). A stopped sandbox with no such snapshot
// keeps the mark.
func (r *Reconciler) sealRestored(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string, act store.GetAgentRunActivityRow, tune orgsettings.Settings) (bool, error) {
	if _, ok := execution.ReverterOf(r.backend); !ok || !restoredMark(w.sb) || w.sb.RestoreCheckpointID.Valid {
		return false, nil
	}
	clear := func() error {
		return r.q.ClearSandboxRestoredMark(ctx, store.ClearSandboxRestoredMarkParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: w.sb.SandboxID})
	}
	switch {
	case sb.Phase.IsStopped():
		if snapshotAfterRestore(w.sb) {
			return false, clear()
		}
		return false, nil
	case sb.Phase != execution.PhaseReady || act.HasActive || act.HasDemand || w.keepWarm || !r.idle(w.sb, act, tune):
		return false, nil
	}
	if ok, err := r.stopGate(ctx, w, sb); err != nil {
		return true, err
	} else if !ok {
		return true, r.record(ctx, w, sb, hash, "waiting for the home backup before the snapshot of the restored home")
	}
	stopped, err := r.stopSnapshot(ctx, w, ws, name)
	if err != nil {
		return true, err
	}
	// The snapshot time is recorded first: a failed clear is then tried again with the time in place.
	sent := stopped.Phase.IsStopped()
	sandboxID := w.sb.SandboxID
	if err := r.record(ctx, w, stopped, hash, ""); err != nil {
		return true, err
	}
	if sent {
		if err := r.q.ClearSandboxRestoredMark(ctx, store.ClearSandboxRestoredMarkParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sandboxID}); err != nil {
			return true, err
		}
		r.log.Info("suspended a sandbox to snapshot its restored home", w.logAttrs()...)
	}
	return true, nil
}

// revertCrashed recovers a crashed sandbox (phase Error). handled is false when the caller must run the
// repair path (the delete and the restore from the checkpoint).
func (r *Reconciler) revertCrashed(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string) (handled bool, err error) {
	if sb.Phase != execution.PhaseError {
		return false, nil
	}
	// A revert does not fix an image that cannot be pulled: the repair path (a new sandbox) handles it.
	// The kind comes from the actor state and the crash message, never from a gRPC code.
	if execution.ClassifySandboxWait(sb.Reason, sb.Message) == execution.WaitCauseImagePull {
		return false, nil
	}
	return r.recoverSandbox(ctx, w, ws, name, sb, hash, causeCrashed)
}

// checkpointWins decides between the last snapshot and the newest complete home checkpoint of the
// sandbox. The newer one wins; a snapshot time that is not known loses. use is false when there is no
// complete checkpoint, so the snapshot is all there is.
//
// The times compared are those of the content, both on the database clock. The checkpoint reads the home
// after its started_at (the claim of its last try; requested_at when a copied row has none), so that is
// its time; finished_at is not: a checkpoint that started before the snapshot and finished after it holds
// the older home. The snapshot time is the clock read before the snapshot call (snapshotClock).
func (r *Reconciler) checkpointWins(ctx context.Context, w *work, sb execution.Sandbox) (cp store.HomeCheckpoint, use bool, err error) {
	if !r.checkpointsOn(w) {
		return store.HomeCheckpoint{}, false, nil // no checkpoint is taken or restored: the snapshot is all there is
	}
	cp, ok, err := r.latestComplete(ctx, w, sb)
	if err != nil || !ok || !cp.FinishedAt.Valid {
		return cp, false, err
	}
	if !w.sb.LastSnapshotAt.Valid {
		return cp, true, nil // the snapshot time is unknown
	}
	taken := cp.RequestedAt
	if cp.StartedAt.Valid {
		taken = cp.StartedAt
	}
	return cp, taken.Time.After(w.sb.LastSnapshotAt.Time), nil
}

// latestComplete is the newest complete home checkpoint of the sandbox, if any.
func (r *Reconciler) latestComplete(ctx context.Context, w *work, sb execution.Sandbox) (store.HomeCheckpoint, bool, error) {
	cp, err := r.q.LatestCompleteHomeCheckpointOfSandbox(ctx, store.LatestCompleteHomeCheckpointOfSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID, SandboxID: sb.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return cp, false, nil
	}
	if err != nil {
		return cp, false, fmt.Errorf("read the latest complete home checkpoint: %w", err)
	}
	return cp, true, nil
}

// pointRestoreAt makes the next sandbox restore the home from the checkpoint.
func (r *Reconciler) pointRestoreAt(ctx context.Context, w *work, cp store.HomeCheckpoint) error {
	row, err := r.q.SetSandboxRestore(ctx, store.SetSandboxRestoreParams{OrgID: w.org.ID, AgentID: w.agent.ID, CheckpointID: cp.ID})
	if err != nil {
		return fmt.Errorf("point the home restore at the checkpoint: %w", err)
	}
	w.sb = row
	return nil
}

// recoverSandbox is the crash path for one sandbox (see the top of this file). handled is false only for
// a crashed sandbox that must go through the repair path.
func (r *Reconciler) recoverSandbox(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string, cause crashCause) (handled bool, err error) {
	rv, ok := execution.ReverterOf(r.backend)
	if !ok || w.agent.UnhealthyAt.Valid {
		return false, nil
	}
	id := w.agent.ID
	now := r.cfg.Now()
	resumed := w.agent.ResumedAt
	detail := "phase " + string(sb.Phase)
	if m := truncate(sb.Message, 300); m != "" {
		detail += ": " + m
	}
	if cause == causeFront {
		detail = "the guest front does not answer"
	}
	if n := r.crash.count(id, now, resumed); n >= maxReverts {
		return true, r.giveUp(ctx, w, sb, hash, name, fmt.Sprintf("the front kept failing: %d reverts in %s; the last failure: %s", n, revertWindow, detail), n)
	}
	if r.crash.backingOff(id, now, resumed) {
		return true, r.record(ctx, w, sb, hash, "")
	}

	// The restore pointer comes first: a revert with a mark and no pointer would lose the home, and so
	// would a repair (a new sandbox with no restore). So it comes before every way out below.
	row, err := r.q.RestoreFromMark(ctx, store.RestoreFromMarkParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	switch {
	case err == nil:
		w.sb = row
		r.log.Info("pointed the home restore at the checkpoint again before the revert", w.logAttrs()...)
	case !errors.Is(err, pgx.ErrNoRows):
		return true, fmt.Errorf("point the home restore at the checkpoint again: %w", err)
	}

	// A revert that failed too often in a row ends here: the repair path takes the sandbox.
	if n := r.crash.failed(id, resumed); n >= maxRevertFailures {
		r.log.Warn("the revert failed at every try; the sandbox goes through the repair path", append(w.logAttrs(), "tries", n)...)
		r.crash.quiet(id)
		return r.repairInstead(ctx, w, ws, name, sb, hash, cause)
	}

	if !restoredMark(w.sb) {
		// The newer of the snapshot and the checkpoint wins.
		cp, use, err := r.checkpointWins(ctx, w, sb)
		if err != nil {
			return true, err
		}
		if use {
			r.log.Warn("a home checkpoint is newer than the last snapshot (or the snapshot time is unknown); recreating the sandbox from it",
				append(w.logAttrs(), "checkpoint", cp.ID.String())...)
			if err := r.pointRestoreAt(ctx, w, cp); err != nil {
				return true, err
			}
			return r.repairInstead(ctx, w, ws, name, sb, hash, cause)
		}
	}

	res, err := rv.RevertSandbox(ctx, ws, name)
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrNoSnapshot):
		// The actor never suspended: a revert would give an empty home. The restore pointer (from the
		// mark) or the newest checkpoint is the home; repair recreates the sandbox.
		if cp, found, cerr := r.latestComplete(ctx, w, sb); cerr != nil {
			return true, cerr
		} else if found && !w.sb.RestoreCheckpointID.Valid {
			if err := r.pointRestoreAt(ctx, w, cp); err != nil {
				return true, err
			}
		}
		return r.repairInstead(ctx, w, ws, name, sb, hash, cause)
	case errors.Is(err, execution.ErrRevertResume):
		// The revert worked and the resume (or the egress write) did not. The snapshot is good: the
		// repair path must not delete it. Count the revert, so the cap stops a front that always
		// fails, and let the next pass read the state.
		r.crash.noteRevert(id, now)
		r.crash.notReady(id)
		r.log.Warn("the sandbox was reverted and did not resume", append(w.logAttrs(), "err", err)...)
		return true, r.record(ctx, w, sb, hash, "reverted to the last snapshot; the resume did not work yet")
	default:
		// RevertActor did not work (no worker has room, the control API failed): back off, and after
		// maxRevertFailures tries repair takes over.
		n := r.crash.noteFailure(id, now, resumed)
		r.log.Warn("the revert failed", append(w.logAttrs(), "err", err, "tries", n)...)
		return true, r.record(ctx, w, sb, hash, "")
	}
	r.crash.noteRevert(id, now)
	r.crash.notReady(id)
	lost := "the writes after the last snapshot (its time is unknown)"
	event := map[string]any{"sandbox": name, "crash": truncate(sb.Message, 300), "cause": detail}
	if w.sb.LastSnapshotAt.Valid {
		at := w.sb.LastSnapshotAt.Time.UTC().Format(time.RFC3339)
		lost = "the writes after " + at
		event["snapshot_at"] = at
	}
	r.log.Warn("reverted a sandbox to its last snapshot", append(w.logAttrs(), "lost", lost, "detail", detail)...)
	if err := audit.RecordAfter(ctx, r.db, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-revert"), Action: "sandbox.revert", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: event,
	}); err != nil {
		return true, err
	}
	return true, r.record(ctx, w, res, hash, "reverted a sandbox to its last snapshot; lost: "+lost)
}

// repairInstead sends the sandbox through the repair path. A crashed sandbox goes back to the caller
// (handled false), which runs the repair when no run holds it. A sandbox whose front is dead (phase
// Ready) is repaired here.
func (r *Reconciler) repairInstead(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string, cause crashCause) (bool, error) {
	if cause == causeCrashed {
		return false, nil
	}
	return true, r.repair(ctx, w, ws, name, sb, hash, homeDirtiedSince(w.sb))
}

// giveUp marks the agent unhealthy after the revert cap.
func (r *Reconciler) giveUp(ctx context.Context, w *work, sb execution.Sandbox, hash, name, reason string, reverts int) error {
	if _, err := r.q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: w.org.ID, ID: w.agent.ID, Reason: reason}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.record(ctx, w, sb, hash, "")
		}
		return err
	}
	r.m.AgentsUnhealthy.Inc()
	r.log.Error("the sandbox keeps failing after a revert; the agent is marked unhealthy", append(w.logAttrs(), "reverts", reverts, "reason", reason)...)
	if err := audit.RecordAfter(ctx, r.db, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-revert"), Action: "agent.mark_unhealthy", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: map[string]any{"sandbox": name, "reason": reason, "reverts": reverts},
	}); err != nil {
		return err
	}
	return r.record(ctx, w, sb, hash, "")
}
