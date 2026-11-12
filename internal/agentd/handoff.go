// SPDX-License-Identifier: Apache-2.0

package agentd

// The handoff of a fresh session (plan item AG1; ADR 0051 amendment 2026-10-24).
//
// A task whose agent version was activated with "apply to open tasks" starts a new harness
// session at its next run. The new session knows nothing of the task, so its first prompt
// carries what Koinon kept: the task's title, the agent's latest end-of-turn summary, its last
// three replies, every message of the people on the task and the outcomes of approvals not yet
// sent. The whole prompt is capped at about 8,000 tokens, estimated as bytes / 3; when it is
// longer, the oldest replies go first, then the oldest messages, then the summary is cut, then
// the newest message. The newest message stays as long as the fixed parts (title, notice,
// outcomes) leave room for it; when they alone are over the cap, the whole prompt is cut.

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Handoff limits.
const (
	handoffMaxTokens = 8000
	handoffMaxBytes  = handoffMaxTokens * bytesPerToken
	handoffReplies   = 3
)

// kindSessionRestarted is the task event of a run that started a fresh session because a new
// agent version was applied to the task.
const kindSessionRestarted = "run.session_restarted"

// sessionRestartedMessage is its message, for the task's timeline.
const sessionRestartedMessage = "Session restarted: new instructions."

// handoff is what a fresh session is told.
type handoff struct {
	Title    string
	Notice   string   // the MCP notice, if any
	Summary  string   // the agent's latest note
	Replies  []string // the agent's last replies, oldest first
	Messages []string // the people's messages, oldest first
	Outcomes string   // approval outcomes not yet sent (build puts parkResumeNote before them), if any
}

// build renders the handoff within handoffMaxBytes.
func (h handoff) build() string {
	replies, messages, summary := h.Replies, h.Messages, h.Summary
	for {
		out := h.render(summary, replies, messages)
		over := len(out) - handoffMaxBytes
		if over <= 0 {
			return out
		}
		switch {
		case len(replies) > 0:
			replies = replies[1:]
		case len(messages) > 1:
			messages = messages[1:]
		case summary != "":
			summary = cutMarked(summary, len(summary)-over)
		case len(messages) == 1 && messages[0] != "":
			// Only the newest message and the fixed parts are left; cut the message.
			if m := cutMarked(messages[0], len(messages[0])-over); m != "" {
				messages = []string{m}
			} else {
				messages = nil
			}
		default:
			// The fixed parts alone are too long (a huge title or outcome), or there is no
			// message to cut (a fresh session on a task without one): cut the whole.
			return cutMarked(out, handoffMaxBytes)
		}
	}
}

func (h handoff) render(summary string, replies, messages []string) string {
	var b strings.Builder
	b.WriteString("Task: " + h.Title)
	b.WriteString("\n\n[Koinon] Your session on this task was restarted because your instructions changed. " +
		"You do not remember the earlier turns; this is what Koinon kept of them. Follow your current instructions.")
	if h.Notice != "" {
		b.WriteString("\n\n" + h.Notice)
	}
	if summary != "" {
		b.WriteString("\n\nYour notes from your last turn:\n" + summary)
	}
	if len(replies) > 0 {
		b.WriteString("\n\nYour last replies on the task, oldest first:")
		for _, r := range replies {
			b.WriteString("\n---\n" + r)
		}
	}
	if len(messages) > 0 {
		b.WriteString("\n\nMessages from the people on the task, oldest first; the last one is the one to act on:")
		for _, m := range messages {
			b.WriteString("\n---\n" + m)
		}
	}
	if h.Outcomes != "" {
		b.WriteString("\n\n" + parkResumeNote + h.Outcomes)
	}
	return b.String()
}

// truncatedMark ends a part of the handoff that build cut.
const truncatedMark = "\n[truncated]"

// cutMarked cuts s to at most n bytes, mark included, on a character boundary and ends it with
// truncatedMark; it returns "" when not even the mark fits.
func cutMarked(s string, n int) string {
	if n < len(truncatedMark) {
		return ""
	}
	return cutBytes(s, n-len(truncatedMark)) + truncatedMark
}

// cutBytes cuts s to at most n bytes on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// messageText is the "text" of a task message event, "" if it has none.
func messageText(e store.TaskEvent) string {
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(e.Payload, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Text)
}

// handoffPrompt builds a fresh session's first prompt. It returns the prompt and the timeline seq it
// covers (the last user message, never going back).
func (r *runner) handoffPrompt(ctx context.Context, task store.Task, outcomes, notice string) (string, int64, error) {
	msgs, err := r.q.ListTaskMessagesAfter(ctx, store.ListTaskMessagesAfterParams{
		OrgID: r.run.OrgID, TaskID: task.ID, AfterSeq: 0,
	})
	if err != nil {
		return "", 0, err
	}
	recent, err := r.q.ListRecentAgentReplies(ctx, store.ListRecentAgentRepliesParams{
		OrgID: r.run.OrgID, TaskID: task.ID, MaxRows: handoffReplies,
	})
	if err != nil {
		return "", 0, err
	}
	h := handoff{Title: task.Title, Notice: notice, Summary: strings.TrimSpace(task.AgentSummary.String), Outcomes: outcomes}
	through := r.run.PromptThroughSeq.Int64
	for _, m := range msgs {
		if t := messageText(m); t != "" {
			h.Messages = append(h.Messages, t)
		}
		through = max(through, m.Seq)
	}
	for i := len(recent) - 1; i >= 0; i-- { // newest first from the query
		if t := messageText(recent[i]); t != "" {
			h.Replies = append(h.Replies, t)
		}
	}
	return h.build(), through, nil
}
