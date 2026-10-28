// SPDX-License-Identifier: Apache-2.0

// Package questions is the state machine behind the ask_user tool (plan item TK3; ADR 0065
// decisions 8 and 10a, ADR 0029 and 0051 for parking and resuming).
//
// A question goes through
//
//	asked    the tool recorded it while the agent's turn runs; nobody sees it yet
//	open     the turn ended: agentd opened it in the transaction that parks the run (reason
//	         input_required) and moves the task to input-required (OpenForRun)
//	answered a person chose a choice, accepted the draft or wrote text (Answer)
//	dismissed a person dismissed it; its safe default is the answer (Dismiss)
//	expired  nobody answered in time; its safe default is the answer (Expire, called by TK4)
//	canceled the run ended (canceled, failed, finished) while it was asked or open; the sweep
//	         (CancelOfFinishedRuns) closes it, and the inbox hides it meanwhile
//
// Every resolution is one transaction under the run's row lock: the question, the audit event, the
// task event and the run's park (ResolveRun) change together, so an answer resumes the run exactly
// once. The agent gets the outcome as the next prompt of the resumed run (internal/agentd), which
// says when the default was applied. Notifications go out after the commit (Notify).
package questions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Task event kinds.
const (
	KindAsked     = "question.asked"
	KindAnswered  = "question.answered"
	KindDismissed = "question.dismissed"
	KindExpired   = "question.expired"
	KindCanceled  = "question.canceled"
)

// Limits on a question's text.
const (
	MaxQuestionBytes = 4000
	MaxDraftBytes    = 8000
	MaxAnswerBytes   = 4000
	MaxChoices       = 10
	MaxChoiceBytes   = 200
)

// DefaultTTL is how long a question waits before it expires (ADR 0065 decision 10: 7 days), and
// DefaultReminder how long before the person is reminded (24 hours). Agents and templates set their
// own (agentconfig.Config.QuestionTimers).
const (
	DefaultTTL      = agentconfig.DefaultQuestionExpiry
	DefaultReminder = agentconfig.DefaultQuestionReminder
)

// Errors.
var (
	// ErrNotOpen: the question was already answered, dismissed or expired, or is not yet visible.
	ErrNotOpen = errors.New("question is not open")
	// ErrNotAskedTo: only the person a question is addressed to may resolve it.
	ErrNotAskedTo = errors.New("this question is addressed to someone else")
	// ErrNotMember: the person named is not a member of the org.
	ErrNotMember = errors.New("no such member in this org")
	// ErrInvalid wraps a refusal of the question's or the answer's content.
	ErrInvalid = errors.New("invalid")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Ask is a question as the agent made it.
type Ask struct {
	Org, Task, Run, Agent pgtype.UUID
	To                    pgtype.UUID
	Question              string
	Choices               []string
	Draft                 string
	// Default is the safe default: the answer used when the question is dismissed or expires.
	Default string
	// ExpiresAt defaults to DefaultTTL from now.
	ExpiresAt time.Time
	// RemindAt is when the person gets a reminder (ADR 0065 decision 10); zero means none. It must
	// come before ExpiresAt.
	RemindAt time.Time
}

// Validate checks the question's content.
func (a *Ask) Validate() error {
	a.Question = strings.TrimSpace(a.Question)
	a.Default = strings.TrimSpace(a.Default)
	switch {
	case a.Question == "":
		return invalid("question is required")
	case len(a.Question) > MaxQuestionBytes || !utf8.ValidString(a.Question):
		return invalid("question must be valid UTF-8 and at most %d bytes", MaxQuestionBytes)
	case a.Default == "":
		return invalid("default is required: name what Koinon should tell you if nobody answers or the question is dismissed")
	case len(a.Default) > MaxAnswerBytes || !utf8.ValidString(a.Default):
		return invalid("default must be valid UTF-8 and at most %d bytes", MaxAnswerBytes)
	case len(a.Draft) > MaxDraftBytes || !utf8.ValidString(a.Draft):
		return invalid("draft must be valid UTF-8 and at most %d bytes", MaxDraftBytes)
	case len(a.Choices) > MaxChoices:
		return invalid("at most %d choices", MaxChoices)
	}
	seen := map[string]bool{}
	for i, c := range a.Choices {
		c = strings.TrimSpace(c)
		a.Choices[i] = c
		if c == "" || len(c) > MaxChoiceBytes || !utf8.ValidString(c) {
			return invalid("each choice must be non-empty text of at most %d bytes", MaxChoiceBytes)
		}
		if seen[c] {
			return invalid("choice %q is listed twice", c)
		}
		seen[c] = true
	}
	if len(a.Choices) > 0 && !slices.Contains(a.Choices, a.Default) {
		return invalid("default must be one of the choices when choices are given")
	}
	if !a.RemindAt.IsZero() && !a.ExpiresAt.IsZero() && !a.RemindAt.Before(a.ExpiresAt) {
		return invalid("the reminder must come before the expiry")
	}
	return nil
}

// callHash identifies the call: the same question to the same person in one run is one question.
func (a Ask) callHash() string {
	b, _ := json.Marshal([]any{a.To.String(), a.Question, a.Choices, a.Draft, a.Default})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Create records the question as asked, or returns the existing one for the same call in the same
// run. It does not change the task or the run: agentd opens the question and parks the run when
// the turn ends. The person must be a member of the org.
func Create(ctx context.Context, q *store.Queries, a Ask) (store.Question, bool, error) {
	if err := a.Validate(); err != nil {
		return store.Question{}, false, err
	}
	member, err := q.IsOrgMember(ctx, store.IsOrgMemberParams{OrgID: a.Org, UserID: a.To})
	if err != nil {
		return store.Question{}, false, err
	}
	if !member {
		return store.Question{}, false, ErrNotMember
	}
	choices, _ := json.Marshal(append([]string{}, a.Choices...))
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = time.Now().Add(DefaultTTL)
	}
	row, err := q.CreateQuestion(ctx, store.CreateQuestionParams{
		OrgID: a.Org, TaskID: a.Task, RunID: a.Run, AgentID: a.Agent, AskedTo: a.To,
		Question: a.Question, Choices: choices, Draft: pgtype.Text{String: a.Draft, Valid: a.Draft != ""},
		DefaultAnswer: a.Default, CallSha256: a.callHash(),
		ExpiresAt: pgtype.Timestamptz{Time: a.ExpiresAt, Valid: true},
		RemindAt:  pgtype.Timestamptz{Time: a.RemindAt, Valid: !a.RemindAt.IsZero()},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = q.GetQuestionByCall(ctx, store.GetQuestionByCallParams{OrgID: a.Org, RunID: a.Run, CallSha256: a.callHash()})
		return row, false, err
	}
	return row, err == nil, err
}

// ChoicesOf decodes a question's choices.
func ChoicesOf(row store.Question) []string {
	var out []string
	_ = json.Unmarshal(row.Choices, &out)
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// OpenForRun opens the run's asked questions: they appear in their addressees' inboxes. Call it in
// the transaction that parks the run and moves the task to input-required, so the park, the task's
// state and the inbox items commit together. It appends a question.asked task event for each.
func OpenForRun(ctx context.Context, q *store.Queries, org, run, task pgtype.UUID) ([]store.Question, error) {
	opened, err := q.OpenAskedQuestionsForRun(ctx, store.OpenAskedQuestionsForRunParams{OrgID: org, RunID: run})
	if err != nil {
		return nil, err
	}
	for _, row := range opened {
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: org, TaskID: task, RunID: run, Kind: KindAsked, ActorKind: store.TaskActorKindAgent,
			ActorAgentID: row.AgentID,
			Payload: mustJSON(map[string]any{
				"question_id": row.ID.String(), "to": row.AskedTo.String(), "question": row.Question,
				"choices": ChoicesOf(row), "has_draft": row.Draft.Valid, "default": row.DefaultAnswer,
				"expires_at": row.ExpiresAt.Time,
			}),
		}); err != nil {
			return nil, err
		}
	}
	return opened, nil
}

// Answer is a person's answer.
type Answer struct {
	// Kind is how the answer was given: choice, draft (accepted as offered) or text.
	Kind store.QuestionAnswerKind
	// Text is the chosen choice, the draft (as accepted or edited) or free text.
	Text string
}

// check validates an answer against the question.
func (a Answer) check(row store.Question) (Answer, error) {
	a.Text = strings.TrimSpace(a.Text)
	switch a.Kind {
	case store.QuestionAnswerKindChoice:
		if !slices.Contains(ChoicesOf(row), a.Text) {
			return a, invalid("%q is not one of the choices", a.Text)
		}
	case store.QuestionAnswerKindDraft:
		if !row.Draft.Valid {
			return a, invalid("this question has no draft")
		}
		if a.Text == "" {
			a.Text = row.Draft.String
		}
	case store.QuestionAnswerKindText:
	default:
		return a, invalid("an answer is a choice, the draft or text")
	}
	switch {
	case a.Text == "":
		return a, invalid("the answer is empty")
	case len(a.Text) > MaxAnswerBytes || !utf8.ValidString(a.Text):
		return a, invalid("the answer must be valid UTF-8 and at most %d bytes", MaxAnswerBytes)
	}
	return a, nil
}

// resolve moves the open question to its end state, with the task event, the audit event and the
// run's park, in the caller's transaction. The caller has not locked anything: it locks the run
// first, then the question, in the same order as agentd's endTurn.
func resolve(ctx context.Context, q *store.Queries, org, id pgtype.UUID, by pgtype.UUID, state store.QuestionState,
	check func(store.Question) (Answer, error),
) (store.Question, error) {
	cur, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return store.Question{}, err
	}
	// The run's lock orders this against agentd's turn end and against approval decisions.
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: cur.RunID})
	if err != nil {
		return store.Question{}, err
	}
	// A question outlives no run: the sweep cancels it, and nothing can resolve it meanwhile.
	if run.State == store.RunStateSucceeded || run.State == store.RunStateFailed || run.State == store.RunStateCanceled {
		return cur, ErrNotOpen
	}
	cur, err = q.LockQuestion(ctx, store.LockQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return store.Question{}, err
	}
	if cur.State != store.QuestionStateOpen {
		return cur, ErrNotOpen
	}
	if by.Valid && by != cur.AskedTo {
		return cur, ErrNotAskedTo
	}
	ans, err := check(cur)
	if err != nil {
		return cur, err
	}
	row, err := q.ResolveQuestion(ctx, store.ResolveQuestionParams{
		OrgID: org, ID: id, State: state, Answer: pgtype.Text{String: ans.Text, Valid: true},
		AnswerKind: ans.Kind, AnsweredBy: by,
	})
	if err != nil {
		return cur, err
	}
	kind, action, actor := KindAnswered, "question.answer", audit.System()
	switch state {
	case store.QuestionStateDismissed:
		kind, action = KindDismissed, "question.dismiss"
	case store.QuestionStateExpired:
		kind, action = KindExpired, "question.expire"
	}
	ev := store.AppendTaskEventParams{
		OrgID: org, TaskID: row.TaskID, RunID: row.RunID, Kind: kind, ActorKind: store.TaskActorKindSystem,
		Payload: mustJSON(map[string]any{
			"question_id": row.ID.String(), "answer_kind": string(ans.Kind), "answer": ans.Text,
			"default_applied": ans.Kind == store.QuestionAnswerKindDefault,
		}),
	}
	if by.Valid {
		actor = audit.User(by)
		ev.ActorKind, ev.ActorUserID = store.TaskActorKindUser, by
	}
	if _, err := q.AppendTaskEvent(ctx, ev); err != nil {
		return cur, err
	}
	if err := audit.RecordErr(ctx, q, audit.Event{
		Org: org, Actor: actor, Action: action, TargetKind: "question", TargetID: row.ID.String(),
		After: map[string]any{
			"state": string(row.State), "answerKind": string(ans.Kind), "runId": row.RunID.String(),
			"taskId": row.TaskID.String(), "agentId": row.AgentID.String(),
		},
	}); err != nil {
		return cur, err
	}
	// Resumes the run, once, when nothing else it waits for is open.
	return row, approvals.ResolveRun(ctx, q, org, row.RunID)
}

// Respond records a person's answer and resumes the run. Only the person the question is addressed
// to may answer. An answer to a question that is no longer open returns ErrNotOpen and changes
// nothing.
func Respond(ctx context.Context, q *store.Queries, org, id, by pgtype.UUID, ans Answer) (store.Question, error) {
	return resolve(ctx, q, org, id, by, store.QuestionStateAnswered, ans.check)
}

func defaultOf(row store.Question) (Answer, error) {
	return Answer{Kind: store.QuestionAnswerKindDefault, Text: row.DefaultAnswer}, nil
}

// Dismiss dismisses the question: its safe default is the answer, and the agent is told so. Only
// the person it is addressed to may dismiss it.
func Dismiss(ctx context.Context, q *store.Queries, org, id, by pgtype.UUID) (store.Question, error) {
	return resolve(ctx, q, org, id, by, store.QuestionStateDismissed, func(r store.Question) (Answer, error) { return defaultOf(r) })
}

// Expire applies the question's safe default because nobody answered in time (TK4 calls it from its
// sweep, in its own transaction per question). No person is the actor.
func Expire(ctx context.Context, q *store.Queries, org, id pgtype.UUID) (store.Question, error) {
	return resolve(ctx, q, org, id, pgtype.UUID{}, store.QuestionStateExpired, func(r store.Question) (Answer, error) { return defaultOf(r) })
}

// CancelForRun cancels the run's asked and open questions, with a question.canceled task event each,
// and returns how many it canceled.
func CancelForRun(ctx context.Context, q *store.Queries, org, run pgtype.UUID, reason string) (int, error) {
	rows, err := q.CancelOpenQuestionsForRun(ctx, store.CancelOpenQuestionsForRunParams{OrgID: org, RunID: run})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: org, TaskID: row.TaskID, RunID: row.RunID, Kind: KindCanceled, ActorKind: store.TaskActorKindSystem,
			Payload: mustJSON(map[string]any{"question_id": row.ID.String(), "reason": reason}),
		}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// CancelOfFinishedRuns cancels the asked and open questions of every run that has ended, with a
// question.canceled task event each, and returns how many it canceled. A question outlives no run:
// cancel, run caps, a ledger budget cancel and archiving an agent all end a parked run without
// touching its questions, so the sweeper calls this (like the approvals sweep's cancelPending).
func CancelOfFinishedRuns(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	q := store.New(pool)
	orgs, err := q.ListOrgsWithQuestionsOfFinishedRuns(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	for _, org := range orgs {
		runs, err := q.ListQuestionRunsOfFinishedRuns(ctx, org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, run := range runs {
			n := 0
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				tq := store.New(tx)
				if _, err := tq.LockRun(ctx, store.LockRunParams{OrgID: org, ID: run}); err != nil {
					return err
				}
				var err error
				n, err = CancelForRun(ctx, tq, org, run, "the run ended before an answer")
				return err
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("cancel questions of run %s: %w", run, err))
				continue
			}
			total += n
		}
	}
	return total, errors.Join(errs...)
}
