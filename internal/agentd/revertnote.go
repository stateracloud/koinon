// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
)

// The revert notice of a run (issue #1526, follow-up of #1481 decision 7).
//
// The reconciler reverts a crashed sandbox to its last snapshot. It is not the owner of a run, so it
// writes no run event: it counts the revert on the sandbox row (NoteSandboxRevert). The owner of the run
// (this worker, at its epoch) reads the row on every heartbeat and writes one run.sandbox_reverted
// event for a revert that happened after the run started and that it did not report yet. The event
// holds the revert count, the snapshot time (absent when it is not known) and the loss: the writes
// after the snapshot are gone.
//
// A revert is reported once. The owner starts from the newest count that an event of the run holds, in
// every epoch, so a worker that takes the run over does not report a revert twice. Reverts between two
// heartbeats give one event, with the newest count and snapshot time. A run whose owner is fenced
// writes nothing (Record returns ErrFenced), and its successor reports the revert from the same row.

// sandboxRevertedKind is the run event kind. It is a note: nothing reads it back to decide state.
const sandboxRevertedKind = "run.sandbox_reverted"

// revertNotice reports a revert of the agent's sandbox to the run, once. reported is the newest count
// this run has reported so far.
type revertNotice struct {
	reported int32
	loaded   bool
}

// noteRevert is one check of the sandbox row. It never fails the run: a read or write that does not
// work is tried again at the next heartbeat.
func (r *runner) noteRevert(ctx context.Context, n *revertNotice) {
	if !n.loaded {
		reported, err := r.q.LastRunSandboxRevertReported(ctx, store.LastRunSandboxRevertReportedParams{OrgID: r.run.OrgID, RunID: r.run.ID})
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading the reported sandbox reverts failed", "err", err)
			}
			return
		}
		n.reported, n.loaded = reported, true
	}
	sb, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
	if err != nil {
		return
	}
	if sb.RevertCount <= n.reported {
		return
	}
	// A revert from before the run started is not a loss of this run. The run's start and the revert
	// time come from the database clock.
	if !sb.RevertedAt.Valid || (r.run.StartedAt.Valid && sb.RevertedAt.Time.Before(r.run.StartedAt.Time)) {
		n.reported = sb.RevertCount
		return
	}
	payload := map[string]any{
		"revert": sb.RevertCount,
		"at":     sb.RevertedAt.Time.UTC().Format(time.RFC3339),
		"lost":   "the writes after the last snapshot of the sandbox",
	}
	if sb.RevertSnapshotAt.Valid {
		at := sb.RevertSnapshotAt.Time.UTC().Format(time.RFC3339)
		payload["snapshot_at"] = at
		payload["lost"] = "the writes after " + at
	}
	if err := r.own.Record(ctx, sandboxRevertedKind, payload); err != nil {
		if ctx.Err() == nil && !r.own.IsFenced() {
			r.log.Warn("recording the sandbox revert failed", "err", err)
		}
		return
	}
	n.reported = sb.RevertCount
}
