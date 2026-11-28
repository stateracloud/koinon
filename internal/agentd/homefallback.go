// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"sort"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
)

// The home fallback notice of a run (issue #1538, same pattern as the revert notice of issue #1526).
//
// A home restore tries the newest complete checkpoint first. When that checkpoint fails (its digest
// does not match, its repository cannot be opened or its restore fails), the restore uses an older one,
// and the work done after the older checkpoint is lost. This worker owns the run, so it writes one
// run.home_fell_back event after the restore succeeds. The event names each skipped checkpoint, its
// time and why it was skipped, and says what was lost. run.home_restored stays as the diagnostic record
// of the restore itself. A restore that fails on every checkpoint fails the run, and its reason names
// the errors: no fallback event is written.

// homeFellBackKind is the run event kind. It is a note: nothing reads it back to decide state.
const homeFellBackKind = "run.home_fell_back"

// Why a checkpoint was skipped. The values are part of the event payload (the web timeline shows them).
const (
	skipDigest     = "its files do not match the recorded digest"
	skipRepository = "its repository cannot be opened"
	skipRestore    = "its restore failed"
	// A checkpoint that the restore list hides (ListCompleteHomeCheckpoints) never reaches the restore.
	hiddenRepositoryGone = "its repository was deleted"
	hiddenEpochTooHigh   = "it is from an ownership epoch that no run has seen"
)

// skippedCheckpoint is one checkpoint that a restore did not use.
type skippedCheckpoint struct {
	id      string
	takenAt time.Time
	reason  string
}

func newSkippedCheckpoint(cp store.HomeCheckpoint, reason string) skippedCheckpoint {
	return skippedCheckpoint{id: cp.ID.String(), takenAt: cp.FinishedAt.Time.UTC(), reason: reason}
}

// noteHomeFallback tells the run that the restore used cp and skipped newer checkpoints. A newer
// checkpoint that the restore list hides (its repository was deleted, or its epoch is too high) counts
// too: its work is lost the same way. It does nothing when none was skipped. It never fails the
// restore: a diagnostic event that Record drops is lost, like any diagnostic event.
func (r *runner) noteHomeFallback(ctx context.Context, used store.HomeCheckpoint, skipped []skippedCheckpoint) {
	hidden, err := r.q.ListHiddenNewerHomeCheckpoints(ctx, store.ListHiddenNewerHomeCheckpointsParams{
		OrgID: r.run.OrgID, AgentID: r.run.AgentID, UsedFinishedAt: used.FinishedAt, UsedID: used.ID,
	})
	if err != nil {
		r.log.Warn("listing the hidden newer home checkpoints failed", "err", err)
	}
	for _, h := range hidden {
		reason := hiddenRepositoryGone
		if h.Reason == "epoch_too_high" {
			reason = hiddenEpochTooHigh
		}
		skipped = append(skipped, skippedCheckpoint{id: h.ID.String(), takenAt: h.FinishedAt.Time.UTC(), reason: reason})
	}
	// Newest first, as the restore tried them.
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].takenAt.After(skipped[j].takenAt) })
	if len(skipped) == 0 {
		return
	}
	list := make([]map[string]any, 0, len(skipped))
	for _, s := range skipped {
		list = append(list, map[string]any{
			"checkpoint": s.id,
			"taken_at":   s.takenAt.Format(time.RFC3339),
			"reason":     s.reason,
		})
	}
	usedAt := used.FinishedAt.Time.UTC().Format(time.RFC3339)
	payload := map[string]any{
		"checkpoint": used.ID.String(),
		"taken_at":   usedAt,
		"skipped":    list,
		"lost":       "the writes after " + usedAt,
	}
	if err := r.own.Record(ctx, homeFellBackKind, payload); err != nil {
		r.log.Warn("recording the home fallback failed", "err", err)
	}
}
