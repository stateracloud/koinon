// SPDX-License-Identifier: Apache-2.0

// Package runreply reads what an agent said in a run, for the task's reply and for the note of a run
// stopped at its time limit. koinon-agentd and koinon-api's wall-clock sweep share it.
package runreply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// KindParked is the run event of a park; a reply restarts after it. agentd's kindParked aliases it.
const KindParked = "run.parked"

// MaxSummaryBytes caps the summary in the note (the end-of-turn summary's cap, 6000 bytes).
const MaxSummaryBytes = 6000

const truncated = "\n[truncated]"

// Reply is the agent's message text of the run since its last park, across every epoch.
func Reply(ctx context.Context, q *store.Queries, org, run pgtype.UUID) (string, error) {
	var b strings.Builder
	var after int64
	for {
		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{
			OrgID: org, RunID: run, AfterSeq: after, MaxEvents: 500,
		})
		if err != nil {
			return "", err
		}
		for _, e := range evs {
			after = e.Seq
			if e.Kind == KindParked {
				// Posted when the run parked.
				b.Reset()
				continue
			}
			if e.Kind != "acp.agent_message_chunk" {
				continue
			}
			var u struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(e.Payload, &u) == nil {
				b.WriteString(u.Content.Text)
			}
		}
		if len(evs) < 500 {
			return strings.TrimSpace(b.String()), nil
		}
	}
}

// Truncate trims s and cuts it to max bytes on a character boundary, ending in "[truncated]".
func Truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max - len(truncated)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + truncated
}

const truncatedHead = "[truncated]\n"

// TruncateTail trims s and keeps its last max bytes on a character boundary, starting with
// "[truncated]". It is for text whose end matters most, such as the agent's last reply.
func TruncateTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := len(s) - (max - len(truncatedHead))
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return truncatedHead + strings.TrimSpace(s[cut:])
}

// Human formats a cap for people: "1h", "1h30m", "30m", "45s" (no zero units).
func Human(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "0s"
	}
	h := int64(d / time.Hour)
	m := int64(d % time.Hour / time.Minute)
	s := int64(d % time.Minute / time.Second)
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// TimeLimitNoteBase is the note without a summary (summarySource "none").
func TimeLimitNoteBase(run store.Run) map[string]any {
	return map[string]any{
		"reason":              "wall_clock_cap",
		"message":             "Stopped at the time limit of " + Human(time.Duration(run.MaxWallClockSeconds)*time.Second) + ".",
		"maxWallClockSeconds": run.MaxWallClockSeconds,
		"summarySource":       "none",
	}
}

// TimeLimitNote is the payload of the run.canceled events of a run stopped at its time limit:
// reason "wall_clock_cap", message "Stopped at the time limit of <Human(cap)>.", maxWallClockSeconds,
// summary, summarySource ("reply": the agent's text since its last park; "notes": the task's last
// end-of-turn summary, when the agent said nothing; "none"). The summary is cut to MaxSummaryBytes
// on a character boundary; a long reply keeps its end (the wrap-up reply is last), a long note its start. On an error the note is still returned, with summarySource "none", so
// the caller can use it as the fallback.
func TimeLimitNote(ctx context.Context, q *store.Queries, run store.Run) (map[string]any, error) {
	note := TimeLimitNoteBase(run)
	reply, err := Reply(ctx, q, run.OrgID, run.ID)
	if err != nil {
		return note, fmt.Errorf("read the agent's reply: %w", err)
	}
	if reply != "" {
		note["summary"], note["summarySource"] = TruncateTail(reply, MaxSummaryBytes), "reply"
		return note, nil
	}
	t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: run.OrgID, ID: run.TaskID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return note, fmt.Errorf("read the task's notes: %w", err)
	}
	if err == nil && strings.TrimSpace(t.AgentSummary.String) != "" {
		note["summary"], note["summarySource"] = Truncate(t.AgentSummary.String, MaxSummaryBytes), "notes"
	}
	return note, nil
}
