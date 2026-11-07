// SPDX-License-Identifier: Apache-2.0

package agentd

// The warning before a run limit (old issue 183; ADR 0066 amendment 2026-10-28; ADR 0081).
//
// A run ends at the first of its time limit, its model-request limit and its spend limit. Before
// it does, agentd cancels the turn in progress (session/cancel) and sends the agent a wrap-up
// prompt on the same session:
//
//   - the time limit: WrapUpNotice before the wall-clock cap (5 minutes by default);
//   - the request and spend limits: at WrapUpFraction (80%) of the limit, read from the ledger
//     every UsagePoll while a turn runs.
//
// Whichever limit is nearest, by fraction, names the notice. A turn that starts inside a window (a
// resume, a retry, a takeover) gets the notice prepended instead. The notice is sent once per run
// (runs.wrap_up_notice_at), for the first limit to reach its window. At the limit the run is
// canceled: by agentd or the sweep at the time limit, with a note that holds what the agent last
// said (internal/runreply), and by the ledger at the request and spend limits, with the same note.
//
// The decisions (which limit is in its window, when to cancel, what a late cancel or a lost notice
// means) are internal/agentd/turnpolicy's (ADR 0087). This file is what the driver (turn.go) does
// for it: the notice row, its event and metric, and the prompt text.

import (
	"context"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/runreply"
)

// kindWrapUpNotice is the run event of the notice: {limit, remainingSeconds, maxWallClockSeconds}
// for every limit, and {modelRequests, maxModelRequests} or {spentUSD, maxSpendUSD} for the
// limit that named it.
const kindWrapUpNotice = "run.wrap_up_notice"

// capEnd is when the run's wall-clock cap is reached. Parked time does not count against the cap
// (ClaimNextRun adds it to parked_seconds).
func (r *runner) capEnd() time.Time {
	capd := time.Duration(r.run.MaxWallClockSeconds)*time.Second + time.Duration(r.run.ParkedSeconds)*time.Second
	return r.run.StartedAt.Time.Add(capd)
}

// readUsage reads what the ledger says the run has used. A ledger that cannot be read gives none:
// the notice is a courtesy, and the limits themselves are enforced elsewhere.
func (r *runner) readUsage(ctx context.Context) (turnpolicy.Usage, bool) {
	usage, err := runlimits.ReadUsage(ctx, r.q, r.run.OrgID, r.run.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("reading the run's usage for the wrap-up notice failed", "err", err)
		}
		return turnpolicy.Usage{}, false
	}
	return turnpolicy.Usage{Requests: usage.Requests, Spend: usage.Spend}, true
}

// markNotice sets runs.wrap_up_notice_at; n is the rows changed.
func (r *runner) markNotice(ctx context.Context) (int64, error) {
	n, err := r.q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch,
	})
	if err != nil {
		return 0, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	return n, nil
}

// suppressWrapUp arms no notice for the rest of the run without having sent one: it sets
// wrap_up_notice_at but records no run.wrap_up_notice event and counts no metric, because the
// event and the metric mean that the agent was told.
func (r *runner) suppressWrapUp(ctx context.Context) error {
	_, err := r.markNotice(ctx)
	return err
}

// wrapUpNote is the prompt text.
func wrapUpNote(c turnpolicy.Cause, limit time.Duration) string {
	const rest = "Wrap up now: do not start new work, finish or save what you are doing, and end your " +
		"turn with a reply that says what you finished, what is left and the next step. At the limit " +
		"Koinon stops the run and posts your last reply on the task."
	switch c.Kind {
	case turnpolicy.KindRequests:
		return fmt.Sprintf("[Koinon] This run has made %d of the %d model requests it may make. %s", c.Requests, c.MaxRequests, rest)
	case turnpolicy.KindSpend:
		return fmt.Sprintf("[Koinon] This run has spent $%s of the $%s it may spend. %s", c.Spent, c.MaxSpend, rest)
	}
	mins := max(int(c.Remaining.Round(time.Minute)/time.Minute), 1)
	about := fmt.Sprintf("%d minutes", mins)
	if mins == 1 {
		about = "1 minute"
	}
	return fmt.Sprintf("[Koinon] About %s remain before this run's time limit of %s. %s", about, runreply.Human(limit), rest)
}

// markWrapUp records the notice (MarkRunWrapUpNotice, the run event, the metric). No row (another
// owner sent it, or the run is no longer running) records nothing and returns false.
func (r *runner) markWrapUp(ctx context.Context, c turnpolicy.Cause) (bool, error) {
	n, err := r.markNotice(ctx)
	if err != nil || n == 0 {
		return false, err
	}
	payload := map[string]any{
		"limit": string(c.Kind), "remainingSeconds": int64(max(time.Until(r.capEnd()), 0).Seconds()), "maxWallClockSeconds": r.run.MaxWallClockSeconds,
	}
	switch c.Kind {
	case turnpolicy.KindRequests:
		payload["modelRequests"], payload["maxModelRequests"] = c.Requests, c.MaxRequests
	case turnpolicy.KindSpend:
		payload["spentUSD"], payload["maxSpendUSD"] = c.Spent.String(), c.MaxSpend.String()
	}
	if err := r.own.Record(ctx, kindWrapUpNotice, payload); err != nil {
		return false, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	r.p.cfg.Metrics.WrapUpNotices.WithLabelValues(string(c.Kind)).Inc()
	return true, nil
}
