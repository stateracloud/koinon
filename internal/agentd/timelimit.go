// SPDX-License-Identifier: Apache-2.0

package agentd

// The warning before a run limit (issue #183; ADR 0066 amendment 2026-10-28; ADR 0081).
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

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/runreply"
)

// errWrapUp: the turn was interrupted to tell the agent a run limit is near.
var errWrapUp = errors.New("a run limit is near")

// kindWrapUpNotice is the run event of the notice: {limit, remainingSeconds, maxWallClockSeconds}
// for every limit, and {modelRequests, maxModelRequests} or {spentUSD, maxSpendUSD} for the
// limit that named it.
const kindWrapUpNotice = "run.wrap_up_notice"

// wrapUpCause is the limit a wrap-up notice is about and where the run stands against it.
type wrapUpCause struct {
	kind runlimits.Kind
	// remaining is the time left, for the time limit.
	remaining time.Duration
	// requests and maxRequests, spent and maxSpend, for the request and spend limits.
	requests, maxRequests int64
	spent, maxSpend       money.USD
}

// capEnd is when the run's wall-clock cap is reached. Parked time does not count against the cap
// (ClaimNextRun adds it to parked_seconds).
func (r *runner) capEnd() time.Time {
	capd := time.Duration(r.run.MaxWallClockSeconds)*time.Second + time.Duration(r.run.ParkedSeconds)*time.Second
	return r.run.StartedAt.Time.Add(capd)
}

// countLimits: the run has a request or a spend limit, which the ledger enforces and agentd can
// only read.
func (r *runner) countLimits() bool { return r.run.MaxModelRequests.Valid || r.run.MaxSpendUsd.Valid }

// timeNotice: the time limit is longer than the notice, so a notice before it makes sense.
func (r *runner) timeNotice() bool {
	n := r.p.cfg.WrapUpNotice
	return n > 0 && time.Duration(r.run.MaxWallClockSeconds)*time.Second > n
}

// wantsWrapUp: a notice can still be sent, because a limit has a window and none was sent.
func (r *runner) wantsWrapUp() bool {
	return (r.timeNotice() || (r.p.cfg.WrapUpFraction > 0 && r.countLimits())) && !r.run.WrapUpNoticeAt.Valid && !r.wrapUpSent
}

// timeCause is the time limit's window now: ok when the cap is within WrapUpNotice and not passed.
// A run already past its cap (taken over after its lease expired) gets no notice: its turn is
// cancelled at once, so the agent could not act on it, and the event and the metric must mean the
// agent was told.
func (r *runner) timeCause() (wrapUpCause, bool) {
	remaining := time.Until(r.capEnd())
	return wrapUpCause{kind: runlimits.KindTime, remaining: remaining}, r.timeNotice() && remaining > 0 && remaining <= r.p.cfg.WrapUpNotice
}

// usageCause reads the ledger and returns the request or spend limit that has reached its window,
// the nearer by fraction. A ledger that cannot be read gives none: the notice is a courtesy, and
// the limits themselves are enforced elsewhere.
func (r *runner) usageCause(ctx context.Context) (wrapUpCause, bool) {
	if !r.countLimits() || r.p.cfg.WrapUpFraction <= 0 {
		return wrapUpCause{}, false
	}
	limits, err := runlimits.Of(r.run)
	if err != nil {
		r.log.Warn("reading the run's limits for the wrap-up notice failed", "err", err)
		return wrapUpCause{}, false
	}
	usage, err := runlimits.ReadUsage(ctx, r.q, r.run.OrgID, r.run.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("reading the run's usage for the wrap-up notice failed", "err", err)
		}
		return wrapUpCause{}, false
	}
	var best wrapUpCause
	bestFraction, found := 0.0, false
	consider := func(c wrapUpCause, f float64) {
		if f >= r.p.cfg.WrapUpFraction && (!found || f > bestFraction) {
			best, bestFraction, found = c, f, true
		}
	}
	if limits.MaxModelRequests > 0 {
		consider(wrapUpCause{kind: runlimits.KindRequests, requests: usage.Requests, maxRequests: limits.MaxModelRequests},
			float64(usage.Requests)/float64(limits.MaxModelRequests))
	}
	if limits.MaxSpend > 0 {
		consider(wrapUpCause{kind: runlimits.KindSpend, spent: usage.Spend, maxSpend: limits.MaxSpend}, money.Ratio(usage.Spend, limits.MaxSpend))
	}
	return best, found
}

// wrapUpDue is the limit whose window the run is in now, the nearest of the three; ok is false
// when none is.
func (r *runner) wrapUpDue(ctx context.Context) (wrapUpCause, bool) {
	t, tok := r.timeCause()
	u, uok := r.usageCause(ctx)
	switch {
	case tok && uok:
		// The nearer by fraction of its limit.
		if cap := time.Duration(r.run.MaxWallClockSeconds) * time.Second; cap > 0 && 1-float64(t.remaining)/float64(cap) >= u.fraction() {
			return t, true
		}
		return u, true
	case tok:
		return t, true
	}
	return u, uok
}

// fraction is how far through its limit the cause is, for the count limits.
func (c wrapUpCause) fraction() float64 {
	switch c.kind {
	case runlimits.KindRequests:
		return float64(c.requests) / float64(c.maxRequests)
	case runlimits.KindSpend:
		return money.Ratio(c.spent, c.maxSpend)
	}
	return 0
}

// watchUsage polls the ledger while a turn runs and sends the cause once a request or spend limit
// reaches its window, then returns.
func (r *runner) watchUsage(ctx context.Context, out chan<- wrapUpCause) {
	t := time.NewTicker(r.p.cfg.UsagePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c, ok := r.usageCause(ctx); ok {
				out <- c
				return
			}
		}
	}
}

// markNotice sets runs.wrap_up_notice_at and r.wrapUpSent; n is the rows changed.
func (r *runner) markNotice(ctx context.Context) (int64, error) {
	n, err := r.q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch,
	})
	if err != nil {
		return 0, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	r.wrapUpSent = true
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
func wrapUpNote(c wrapUpCause, limit time.Duration) string {
	const rest = "Wrap up now: do not start new work, finish or save what you are doing, and end your " +
		"turn with a reply that says what you finished, what is left and the next step. At the limit " +
		"Koinon stops the run and posts your last reply on the task."
	switch c.kind {
	case runlimits.KindRequests:
		return fmt.Sprintf("[Koinon] This run has made %d of the %d model requests it may make. %s", c.requests, c.maxRequests, rest)
	case runlimits.KindSpend:
		return fmt.Sprintf("[Koinon] This run has spent $%s of the $%s it may spend. %s", c.spent, c.maxSpend, rest)
	}
	mins := max(int(c.remaining.Round(time.Minute)/time.Minute), 1)
	about := fmt.Sprintf("%d minutes", mins)
	if mins == 1 {
		about = "1 minute"
	}
	return fmt.Sprintf("[Koinon] About %s remain before this run's time limit of %s. %s", about, runreply.Human(limit), rest)
}

// markWrapUp records the notice (MarkRunWrapUpNotice, the run event, the metric) and sets
// r.wrapUpSent. No row (another owner sent it, or the run is no longer running) sets wrapUpSent,
// records nothing and returns false.
func (r *runner) markWrapUp(ctx context.Context, c wrapUpCause) (bool, error) {
	n, err := r.markNotice(ctx)
	if err != nil || n == 0 {
		return false, err
	}
	payload := map[string]any{
		"limit": string(c.kind), "remainingSeconds": int64(max(time.Until(r.capEnd()), 0).Seconds()), "maxWallClockSeconds": r.run.MaxWallClockSeconds,
	}
	switch c.kind {
	case runlimits.KindRequests:
		payload["modelRequests"], payload["maxModelRequests"] = c.requests, c.maxRequests
	case runlimits.KindSpend:
		payload["spentUSD"], payload["maxSpendUSD"] = c.spent.String(), c.maxSpend.String()
	}
	if err := r.record(ctx, kindWrapUpNotice, payload); err != nil {
		return false, r.dbErr(ctx, "record the wrap-up notice", err)
	}
	r.p.cfg.Metrics.WrapUpNotices.WithLabelValues(string(c.kind)).Inc()
	return true, nil
}
