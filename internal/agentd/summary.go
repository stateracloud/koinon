// SPDX-License-Identifier: Apache-2.0

package agentd

// The end-of-turn summary (plan item AG1; ADR 0051 amendment 2026-10-24).
//
// When a turn ends, agentd sends the agent one more prompt on the same session and asks for a
// short note of where things stand and what comes next. The note is stored on the task
// ("Agent's notes") and is what a fresh session starts from when a new agent version is applied
// to open tasks (handoff.go). It is a normal model call through the gateway, so the ledger
// charges it to the run's budget like the turn itself.
//
// The prompt must change nothing:
//
//   - the reply is not a timeline message: the agent's message chunks of the summary prompt are
//     collected here and never recorded as run events, which is what the task's reply is built
//     from;
//   - tools are refused three ways: the prompt says not to use any (harness-local tools such as a shell
//     or file edits do take effect, so it says so); permission requests during
//     it are rejected; and while it runs the run is marked summarizing, which makes Koinon's own
//     tool servers (GitHub, remote MCP servers) answer as if no run were active, so no approval
//     request can be recorded. A harness that calls a tool anyway has its summary discarded and
//     the prompt cancelled, and the run records run.summary_refused.
//   - a summary that fails, times out or is refused never fails the run.

import (
	"context"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runreply"
)

// Sizes. A token is estimated as 3 bytes, the same estimate the handoff uses.
const (
	bytesPerToken      = 3
	summaryMaxTokens   = 2000
	summaryMaxBytes    = summaryMaxTokens * bytesPerToken
	summaryTruncated   = "\n[truncated]"
	defaultSummaryWait = 2 * time.Minute
)

// summaryPrompt is the extra prompt. It starts with a marker tests and logs can recognise.
const summaryPrompt = "[Koinon] Your turn is over. Write a short note to your future self about this task, " +
	"for use if your session is restarted: where things stand, what you decided and why, and the next steps. " +
	"Answer with the note only, in plain text, at most 1500 words. Do not use any tool at all: no shell " +
	"commands, no file edits, no searches, no calls to Koinon or other servers. Tools that run inside your " +
	"sandbox do take effect, so do not use them now. This note is not shown as a reply to the user."

// summaryState collects the summary prompt's reply.
type summaryState struct {
	text    strings.Builder
	toolUse bool
}

// truncateSummary trims s and cuts it to summaryMaxBytes on a character boundary.
func truncateSummary(s string) string { return runreply.Truncate(s, summaryMaxBytes) }

// summarize asks the agent for its note and stores it on the task. It never fails the run.
func (r *runner) summarize(ctx context.Context, conn *acp.Conn, session acp.SessionID) {
	if r.isFenced() || ctx.Err() != nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	n, err := r.q.SetRunSummarizing(ctx, store.SetRunSummarizingParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, Summarizing: true,
	})
	if err != nil || n == 0 {
		r.log.Warn("summary skipped: could not mark the run summarizing", "err", err)
		return
	}
	st := &summaryState{}
	r.sumMu.Lock()
	r.sum = st
	r.sumMu.Unlock()
	defer func() {
		r.sumMu.Lock()
		r.sum = nil
		r.sumMu.Unlock()
		if _, err := r.q.SetRunSummarizing(bg, store.SetRunSummarizingParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, Summarizing: false,
		}); err != nil {
			r.log.Warn("clearing the summarizing mark failed", "err", err)
		}
	}()

	wait := r.p.cfg.SummaryTimeout
	pctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	type result struct {
		stop acp.StopReason
		err  error
	}
	done := make(chan result, 1)
	go func() {
		stop, err := conn.Prompt(pctx, session, summaryPrompt)
		done <- result{stop, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-r.fenced:
		cancel()
		return
	}
	r.sumMu.Lock()
	text, toolUse := st.text.String(), st.toolUse
	r.sumMu.Unlock()
	switch {
	case toolUse:
		r.log.Warn("the agent called a tool while writing its summary; the summary is discarded")
		// Not a tool-less prompt after all: cancel whatever is still going on.
		_ = conn.Cancel(bg, session)
		r.noteSummary("run.summary_refused", map[string]any{"reason": "the agent called a tool"})
	case res.err != nil || res.stop != acp.StopEndTurn:
		reason := string(res.stop)
		if res.err != nil {
			reason = res.err.Error()
		}
		r.log.Warn("the end-of-turn summary failed", "reason", reason)
		_ = conn.Cancel(bg, session)
		r.noteSummary("run.summary_failed", map[string]any{"reason": reason})
	case strings.TrimSpace(text) == "":
		r.noteSummary("run.summary_failed", map[string]any{"reason": "the agent wrote nothing"})
	default:
		out := truncateSummary(text)
		rows, err := r.q.SetTaskSummary(ctx, store.SetTaskSummaryParams{
			OrgID: r.run.OrgID, ID: r.run.TaskID, RunID: r.run.ID, Epoch: r.run.Epoch, Summary: out,
		})
		if err != nil || rows == 0 {
			r.log.Warn("storing the summary failed", "err", err)
			return
		}
		r.noteSummary("run.summary", map[string]any{"bytes": len(out), "truncated": len(strings.TrimSpace(text)) > len(out)})
	}
}

// noteSummary records what became of the summary. A failed write is only logged.
func (r *runner) noteSummary(kind string, payload map[string]any) {
	if err := r.record(context.WithoutCancel(context.Background()), kind, payload); err != nil {
		r.log.Warn("recording the summary outcome failed", "kind", kind, "err", err)
	}
}

// summaryUpdate takes a session update during the summary prompt; it reports whether it did, in
// which case the update is not recorded as a run event.
func (r *runner) summaryUpdate(n acp.SessionNotification) bool {
	r.sumMu.Lock()
	defer r.sumMu.Unlock()
	if r.sum == nil {
		return false
	}
	u := acp.Flatten(n)
	switch u.Kind {
	case "agent_message_chunk":
		if r.sum.text.Len() <= 4*summaryMaxBytes {
			r.sum.text.WriteString(u.Text)
		}
	case "tool_call", "tool_call_update":
		r.sum.toolUse = true
	}
	return true
}

// summarizingNow reports whether the summary prompt is running.
func (r *runner) summarizingNow() bool {
	r.sumMu.Lock()
	defer r.sumMu.Unlock()
	return r.sum != nil
}
