// SPDX-License-Identifier: Apache-2.0

package agentd

// The warning before the time limit (issue #183; ADR 0066 amendment 2026-10-28).
//
// WrapUpNotice before the run's wall-clock cap, agentd cancels the turn in progress (session/cancel)
// and sends the agent a wrap-up prompt on the same session. A turn that starts inside the window
// (a resume, a retry, a takeover) gets the notice prepended instead. The notice is sent once per
// run (runs.wrap_up_notice_at). At the cap the run is canceled with a note that holds what the
// agent last said (internal/runreply).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runreply"
)

// errWrapUp: the turn was interrupted to tell the agent its time limit is near.
var errWrapUp = errors.New("the time limit is near")

// kindWrapUpNotice is the run event of the notice: {remainingSeconds, maxWallClockSeconds}.
const kindWrapUpNotice = "run.wrap_up_notice"

// capEnd is when the run's wall-clock cap is reached. Parked time does not count against the cap
// (ClaimNextRun adds it to parked_seconds).
func (r *runner) capEnd() time.Time {
	capd := time.Duration(r.run.MaxWallClockSeconds)*time.Second + time.Duration(r.run.ParkedSeconds)*time.Second
	return r.run.StartedAt.Time.Add(capd)
}

// wantsWrapUp: a notice is configured, the cap is longer than the notice, and none was sent.
func (r *runner) wantsWrapUp() bool {
	n := r.p.cfg.WrapUpNotice
	return n > 0 && time.Duration(r.run.MaxWallClockSeconds)*time.Second > n && !r.run.WrapUpNoticeAt.Valid && !r.wrapUpSent
}

// wrapUpNote is the prompt text.
func wrapUpNote(remaining, limit time.Duration) string {
	mins := max(int(remaining.Round(time.Minute)/time.Minute), 1)
	about := fmt.Sprintf("%d minutes", mins)
	if mins == 1 {
		about = "1 minute"
	}
	return fmt.Sprintf("[Koinon] About %s remain before this run's time limit of %s. Wrap up now: do not start new "+
		"work, finish or save what you are doing, and end your turn with a reply that says what you "+
		"finished, what is left and the next step. At the limit Koinon stops the run and posts your "+
		"last reply on the task.", about, runreply.Human(limit))
}

// markWrapUp records the notice (MarkRunWrapUpNotice, the run event, the metric) and sets
// r.wrapUpSent. No row (another owner sent it, or the run is no longer running) sets wrapUpSent,
// records nothing and returns false.
func (r *runner) markWrapUp(ctx context.Context, remaining time.Duration) (bool, error) {
	n, err := r.q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch,
	})
	if err != nil {
		return false, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	r.wrapUpSent = true
	if n == 0 {
		return false, nil
	}
	if err := r.record(ctx, kindWrapUpNotice, map[string]any{
		"remainingSeconds": int64(max(remaining, 0).Seconds()), "maxWallClockSeconds": r.run.MaxWallClockSeconds,
	}); err != nil {
		return false, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	r.p.cfg.Metrics.WrapUpNotices.Inc()
	return true, nil
}
