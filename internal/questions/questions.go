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
// task event and the run's park (runpark.Resolve) change together, so an answer resumes the run exactly
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
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runpark"
)

// Task event kinds.
const (
	KindAsked     = "question.asked"
	KindAnswered  = "question.answered"
	KindDismissed = "question.dismissed"
	KindExpired   = "question.expired"
	KindCanceled  = "question.canceled"
	// KindRetargeted: nobody a question was addressed to can answer any more, so the org's admins
	// were added as recipients (RetargetOrphaned).
	KindRetargeted = "question.retargeted"
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
	// ErrNotAskedTo: only a recipient of a question may resolve it.
	ErrNotAskedTo = errors.New("this question is not addressed to you")
	// ErrNotMember: the person named is not a member of the org.
	ErrNotMember = errors.New("no such member in this org")
	// ErrInvalid wraps a refusal of the question's or the answer's content.
	ErrInvalid = errors.New("invalid")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Target is who a question is addressed to (ADR 0065 amendment 2026-10-28).
type Target struct {
	Kind    store.NotifyTargetKind
	User    pgtype.UUID   // kind user
	Role    store.OrgRole // kind role
	Channel string        // kind channel (v0.2)
}

// DefaultTarget is the org's default target: today the org's admins (role admin or higher).
var DefaultTarget = Target{Kind: store.NotifyTargetKindRole, Role: store.OrgRoleAdmin}

// Key identifies the target in the call hash. A user target hashes as before (its UUID string), so
// a question asked before 00036 and asked again after it is still the same question.
func (t Target) Key() string {
	switch t.Kind {
	case store.NotifyTargetKindRole:
		return "role:" + string(t.Role)
	case store.NotifyTargetKindChannel:
		return "channel:" + t.Channel
	}
	return t.User.String()
}

// Label is the target in plain words; name is the user's display name for kind user.
func Label(t Target, name string) string {
	switch t.Kind {
	case store.NotifyTargetKindRole:
		switch t.Role {
		case store.OrgRoleOwner:
			return "the org's owners"
		case store.OrgRoleMember:
			return "the org's members"
		}
		return "the org's admins"
	case store.NotifyTargetKindChannel:
		return "a chat channel"
	}
	return name
}

// Recipients resolves a target to the users who can answer: the person (when still a member with
// role member or higher), or everyone with the role or higher. A channel resolves to the org's admins
// (the inbox item is always created; v0.2 adds the channel message).
func Recipients(ctx context.Context, q *store.Queries, org pgtype.UUID, t Target) ([]pgtype.UUID, error) {
	switch t.Kind {
	case store.NotifyTargetKindUser:
		ok, err := q.IsOrgMember(ctx, store.IsOrgMemberParams{OrgID: org, UserID: t.User})
		if err != nil || !ok {
			return nil, err
		}
		return []pgtype.UUID{t.User}, nil
	case store.NotifyTargetKindRole:
		return q.ListOrgMemberIDsWithRole(ctx, store.ListOrgMemberIDsWithRoleParams{OrgID: org, MinRole: t.Role})
	case store.NotifyTargetKindChannel:
		return q.ListOrgMemberIDsWithRole(ctx, store.ListOrgMemberIDsWithRoleParams{OrgID: org, MinRole: store.OrgRoleAdmin})
	}
	return nil, invalid("unknown target kind %q", t.Kind)
}

// Ask is a question as the agent made it.
type Ask struct {
	Org, Task, Run, Agent pgtype.UUID
	// Target is who the question is addressed to, and Recipients the users it resolved to (see
	// Recipients). It is asked of each of them; the first answer wins.
	Target     Target
	Recipients []pgtype.UUID
	Question   string
	Choices    []string
	Draft      string
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
	case len(a.Recipients) == 0:
		return invalid("nobody in this org can answer it")
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
	b, _ := json.Marshal([]any{a.Target.Key(), a.Question, a.Choices, a.Draft, a.Default})
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
	for _, u := range a.Recipients {
		member, err := q.IsOrgMember(ctx, store.IsOrgMemberParams{OrgID: a.Org, UserID: u})
		if err != nil {
			return store.Question{}, false, err
		}
		if !member {
			return store.Question{}, false, ErrNotMember
		}
	}
	choices, _ := json.Marshal(append([]string{}, a.Choices...))
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = time.Now().Add(DefaultTTL)
	}
	params := store.CreateQuestionParams{
		OrgID: a.Org, TaskID: a.Task, RunID: a.Run, AgentID: a.Agent, TargetKind: a.Target.Kind,
		Question: a.Question, Choices: choices, Draft: pgtype.Text{String: a.Draft, Valid: a.Draft != ""},
		DefaultAnswer: a.Default, CallSha256: a.callHash(),
		ExpiresAt: pgtype.Timestamptz{Time: a.ExpiresAt, Valid: true},
		RemindAt:  pgtype.Timestamptz{Time: a.RemindAt, Valid: !a.RemindAt.IsZero()},
	}
	switch a.Target.Kind {
	case store.NotifyTargetKindUser:
		// asked_to is deprecated (00036): written for a user target, NULL otherwise, never read.
		params.AskedTo, params.TargetUserID = a.Target.User, a.Target.User
	case store.NotifyTargetKindRole:
		params.TargetRole = store.NullOrgRole{OrgRole: a.Target.Role, Valid: true}
	}
	row, err := q.CreateQuestion(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = q.GetQuestionByCall(ctx, store.GetQuestionByCallParams{OrgID: a.Org, RunID: a.Run, CallSha256: a.callHash()})
		return row, false, err
	}
	if err != nil {
		return row, false, err
	}
	if err := q.AddQuestionRecipients(ctx, store.AddQuestionRecipientsParams{OrgID: a.Org, QuestionID: row.ID, UserIds: a.Recipients}); err != nil {
		return row, false, err
	}
	return row, true, nil
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
	if len(opened) > 0 {
		ids := make([]pgtype.UUID, 0, len(opened))
		for _, row := range opened {
			ids = append(ids, row.ID)
		}
		// The questions became visible: their recipients start waiting now.
		if err := q.StartQuestionRecipientsWaiting(ctx, store.StartQuestionRecipientsWaitingParams{OrgID: org, QuestionIds: ids}); err != nil {
			return nil, err
		}
	}
	for _, row := range opened {
		recipients, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: org, QuestionID: row.ID})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(recipients))
		for _, u := range recipients {
			ids = append(ids, u.String())
		}
		target := TargetOf(row)
		name := ""
		if target.Kind == store.NotifyTargetKindUser {
			if name, err = q.GetUserDisplayName(ctx, target.User); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: org, TaskID: task, RunID: run, Kind: KindAsked, ActorKind: store.TaskActorKindAgent,
			ActorAgentID: row.AgentID,
			Payload: mustJSON(map[string]any{
				"question_id": row.ID.String(), "question": row.Question,
				"target":     map[string]any{"kind": string(target.Kind), "user": target.User.String(), "role": string(target.Role)},
				"to_label":   Label(target, name),
				"recipients": ids,
				"choices":    ChoicesOf(row), "has_draft": row.Draft.Valid, "default": row.DefaultAnswer,
				"expires_at": row.ExpiresAt.Time,
			}),
		}); err != nil {
			return nil, err
		}
	}
	return opened, nil
}

// TargetOf is the target a question row was addressed to.
func TargetOf(row store.Question) Target {
	t := Target{Kind: row.TargetKind, User: row.TargetUserID}
	if row.TargetRole.Valid {
		t.Role = row.TargetRole.OrgRole
	}
	if row.TargetChannelID.Valid {
		t.Channel = row.TargetChannelID.String()
	}
	return t
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
// run's park, in the caller's transaction. The caller has not locked anything. A question that is
// already resolved returns ErrNotOpen before any lock. Otherwise resolve locks the run first and
// then the question. The turn end (runoutcome Owner.EndTurn) takes the run's lock before it reads
// or changes anything else of the run; the one question write it makes earlier,
// MarkQuestionsDelivered, only touches questions that are already resolved, and resolve never
// writes those, so the two cannot wait on each other.
func resolve(ctx context.Context, q *store.Queries, b audit.Batch, org, id pgtype.UUID, by pgtype.UUID, state store.QuestionState,
	check func(store.Question) (Answer, error),
) (store.Question, error) {
	cur, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return store.Question{}, err
	}
	// An answer to a question that is already resolved changes nothing: say so without waiting for
	// the run's lock, which a turn end may hold. The check is repeated under the lock below.
	if cur.State != store.QuestionStateOpen {
		return cur, ErrNotOpen
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
	// Read the question again, without a lock, before the question's lock. Every resolve holds the
	// run's lock, and this statement runs after we got it, so under READ COMMITTED it sees every
	// answer committed before that moment. A question that is still open here cannot be a row that
	// MarkQuestionsDelivered has locked (it only locks resolved rows), so LockQuestion below does
	// not wait on a turn end that itself waits on the run's lock, which we hold (no 40P01).
	cur, err = q.GetQuestion(ctx, store.GetQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return store.Question{}, err
	}
	if cur.State != store.QuestionStateOpen {
		return cur, ErrNotOpen
	}
	cur, err = q.LockQuestion(ctx, store.LockQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return store.Question{}, err
	}
	if cur.State != store.QuestionStateOpen {
		return cur, ErrNotOpen
	}
	// Only a recipient who can still answer may resolve it. The first answer wins: the run's lock
	// and then the question's lock order two answers, and the second finds the question no longer
	// open (ErrNotOpen).
	if by.Valid {
		recipient, err := q.IsActiveQuestionRecipient(ctx, store.IsActiveQuestionRecipientParams{OrgID: org, QuestionID: id, UserID: by})
		if err != nil {
			return cur, err
		}
		member, err := q.IsOrgMember(ctx, store.IsOrgMemberParams{OrgID: org, UserID: by})
		if err != nil {
			return cur, err
		}
		if !recipient || !member {
			return cur, ErrNotAskedTo
		}
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
	kind, action, actor := KindAnswered, "question.answer", audit.System("koinon-api:questions")
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
	after := map[string]any{
		"state": string(row.State), "answerKind": string(ans.Kind), "runId": row.RunID.String(),
		"taskId": row.TaskID.String(), "agentId": row.AgentID.String(), "targetKind": string(row.TargetKind),
	}
	if by.Valid {
		actor = audit.User(by)
		ev.ActorKind, ev.ActorUserID = store.TaskActorKindUser, by
		name, err := q.GetUserDisplayName(ctx, by)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return cur, err
		}
		payload := map[string]any{
			"question_id": row.ID.String(), "answer_kind": string(ans.Kind), "answer": ans.Text,
			"default_applied": ans.Kind == store.QuestionAnswerKindDefault, "answered_by_name": name,
		}
		ev.Payload = mustJSON(payload)
	}
	if _, err := q.AppendTaskEvent(ctx, ev); err != nil {
		return cur, err
	}
	b.Org(audit.Event{
		Org: org, Actor: actor, Action: action, TargetKind: "question", TargetID: row.ID.String(),
		After: after,
	})
	// Resumes the run, once, when nothing else it waits for is open.
	return row, runpark.Resolve(ctx, q, org, row.RunID)
}

// Respond records a person's answer and resumes the run. Only a recipient of the question may
// answer, and the first answer wins. An answer to a question that is no longer open returns ErrNotOpen and changes
// nothing.
func Respond(ctx context.Context, q *store.Queries, b audit.Batch, org, id, by pgtype.UUID, ans Answer) (store.Question, error) {
	return resolve(ctx, q, b, org, id, by, store.QuestionStateAnswered, ans.check)
}

func defaultOf(row store.Question) (Answer, error) {
	return Answer{Kind: store.QuestionAnswerKindDefault, Text: row.DefaultAnswer}, nil
}

// Dismiss dismisses the question: its safe default is the answer, and the agent is told so. Only a
// recipient may dismiss it, and a dismissal is an answer: the default applies for every recipient.
func Dismiss(ctx context.Context, q *store.Queries, b audit.Batch, org, id, by pgtype.UUID) (store.Question, error) {
	return resolve(ctx, q, b, org, id, by, store.QuestionStateDismissed, func(r store.Question) (Answer, error) { return defaultOf(r) })
}

// Expire applies the question's safe default because nobody answered in time (TK4 calls it from its
// sweep, in its own transaction per question). No person is the actor.
func Expire(ctx context.Context, q *store.Queries, b audit.Batch, org, id pgtype.UUID) (store.Question, error) {
	return resolve(ctx, q, b, org, id, pgtype.UUID{}, store.QuestionStateExpired, func(r store.Question) (Answer, error) { return defaultOf(r) })
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
