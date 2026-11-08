// SPDX-License-Identifier: Apache-2.0

package questions

// Reminders and expiry (plan item TK4; ADR 0065 decision 10).
//
// An open question gets one in-app reminder for the person asked at remind_at (24 hours after it was
// asked unless the agent's version or template says otherwise), and expires at expires_at (7 days):
// the safe default is applied (Expire) and the parked run resumes with one short "no answer" prompt
// (internal/agentd), so the agent can cancel the task or ask someone else.
//
// Both are driven by a periodic sweep of koinon-api rather than one DBOS sleep per question: the
// deadlines live in the question row, so a sweep after a restart (or on another replica) finds the
// same due questions, and it works with and without api.durable.enabled. The effects happen once:
//   - the reminder is a notification with the ID "question.reminder:<id>" (one DBOS workflow with
//     durable on) whose handler writes a notification row with a unique key and then sets
//     reminded_at;
//   - the expiry is the state change open -> expired under the run's row lock, which a second
//     sweep, a second replica or a late answer finds already made (ErrNotOpen).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

// NotifyReminder is the notification kind that reminds a person of a question that still waits for
// them. Chat messages (Slack, Teams) join the in-app notification with the chat connectors (v0.2).
const NotifyReminder = "question.reminder"

// ReminderNotificationKind is the kind of the in-app notification row.
const ReminderNotificationKind = "question.reminder"

func reminderNotification(row store.Question) durable.Notification {
	p, _ := json.Marshal(payload{Org: row.OrgID.String(), Question: row.ID.String()})
	return durable.Notification{ID: NotifyReminder + ":" + row.ID.String(), Kind: NotifyReminder, Payload: string(p)}
}

// registerReminder registers the handler of the reminder notification on n.
func registerReminder(n *durable.Notifier, pool *pgxpool.Pool) {
	n.Handle(NotifyReminder, func(ctx context.Context, note durable.Notification) error {
		err := deliverReminder(ctx, pool, note)
		if err == nil {
			return nil
		}
		var p payload
		if json.Unmarshal([]byte(note.Payload), &p) == nil {
			var org, id pgtype.UUID
			if org.Scan(p.Org) == nil && id.Scan(p.Question) == nil {
				row, rerr := store.New(pool).RecordQuestionRemindFailure(ctx, store.RecordQuestionRemindFailureParams{OrgID: org, ID: id, GiveUp: giveUpAfter})
				if rerr == nil && row.RemindedAt.Valid {
					slog.Error("question reminder abandoned after repeated failures", "org", p.Org, "question", p.Question, "failures", row.RemindFailures, "err", err)
				}
			}
		}
		return err
	})
}

func deliverReminder(ctx context.Context, pool *pgxpool.Pool, note durable.Notification) error {
	var p payload
	if err := json.Unmarshal([]byte(note.Payload), &p); err != nil {
		return fmt.Errorf("decode %s payload: %w", note.Kind, err)
	}
	var org, id pgtype.UUID
	if err := org.Scan(p.Org); err != nil {
		return fmt.Errorf("org %q: %w", p.Org, err)
	}
	if err := id.Scan(p.Question); err != nil {
		return fmt.Errorf("question %q: %w", p.Question, err)
	}
	q := store.New(pool)
	row, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: org, ID: id})
	if err != nil {
		return err
	}
	// A question answered or expired before this ran needs no reminder.
	if row.State == store.QuestionStateOpen {
		body, _ := json.Marshal(map[string]any{
			"question_id": row.ID.String(), "task_id": row.TaskID.String(), "question": row.Question,
			"expires_at": row.ExpiresAt.Time.UTC().Format(time.RFC3339), "default": row.DefaultAnswer,
			// The notification list shows an unknown kind by its message.
			"message": fmt.Sprintf("Reminder: a question is still waiting for your answer: %s If nobody answers by %s, the default (%s) applies.",
				clip(row.Question, 200), row.ExpiresAt.Time.UTC().Format("2006-01-02 15:04 UTC"), clip(row.DefaultAnswer, 100)),
		})
		if err := notifyRecipients(ctx, q, row, ReminderNotificationKind, "question-reminder:"+row.ID.String(), body); err != nil {
			return err
		}
	}
	_, err = q.MarkQuestionReminded(ctx, store.MarkQuestionRemindedParams{OrgID: org, ID: id})
	return err
}

// SweepResult is what one RunTimers pass did.
type SweepResult struct{ Sent, Expired int }

// RunTimers sends the reminders due at now and expires the questions past their expiry at now, in
// every org. now is a parameter so tests can move the clock. One question or org that keeps failing
// does not block the others: failures are returned joined.
func RunTimers(ctx context.Context, pool *pgxpool.Pool, send durable.Sender, now time.Time) (SweepResult, error) {
	var res SweepResult
	orgs, err := store.New(pool).ListOrgsWithDueQuestions(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if err != nil {
		return res, err
	}
	var errs []error
	for _, org := range orgs {
		r, err := RunOrgTimers(ctx, pool, send, org, now)
		res.Sent += r.Sent
		res.Expired += r.Expired
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// RunOrgTimers is RunTimers for one org.
func RunOrgTimers(ctx context.Context, pool *pgxpool.Pool, send durable.Sender, org pgtype.UUID, now time.Time) (SweepResult, error) {
	var res SweepResult
	q := store.New(pool)
	at := pgtype.Timestamptz{Time: now, Valid: true}
	var errs []error
	// Expire first: a question that expires needs no reminder.
	due, err := q.ListDueQuestions(ctx, store.ListDueQuestionsParams{OrgID: org, Now: at, MaxRows: 100})
	if err != nil {
		return res, err
	}
	for _, row := range due {
		_, err := audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
			_, err := Expire(ctx, q, b, org, row.ID)
			return err
		})
		switch {
		case errors.Is(err, ErrNotOpen):
			// Answered, dismissed or canceled meanwhile, or expired by another replica.
		case err != nil:
			errs = append(errs, fmt.Errorf("expire question %s: %w", row.ID, err))
		default:
			res.Expired++
		}
	}
	remind, err := q.ListQuestionsToRemind(ctx, store.ListQuestionsToRemindParams{OrgID: org, Now: at, MaxRows: 100})
	if err != nil {
		return res, errors.Join(append(errs, err)...)
	}
	// Sent counts reminders handed to the sender, not reminders delivered: with durable on, Send only
	// records the workflow and the handler sets reminded_at later, so until then every sweep sends
	// the same workflow ID again. That is a no-op, but it is counted here again.
	for _, row := range remind {
		if err := send.Send(ctx, reminderNotification(row)); err != nil {
			slog.Error("question reminder failed", "org", org.String(), "question", row.ID.String(), "err", err)
			errs = append(errs, fmt.Errorf("remind question %s: %w", row.ID, err))
			continue
		}
		res.Sent++
	}
	return res, errors.Join(errs...)
}

// clip shortens s to at most n runes, with an ellipsis.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// TaskTarget is the notify target a task carries (copied from its trigger when it was created), or
// the org's default (DefaultTarget, the admins) when it has none.
func TaskTarget(kind store.NullNotifyTargetKind, user pgtype.UUID, role store.NullOrgRole) Target {
	if !kind.Valid {
		return DefaultTarget
	}
	t := Target{Kind: kind.NotifyTargetKind, User: user}
	if role.Valid {
		t.Role = role.OrgRole
	}
	return t
}

// RetargetOrphaned gives the open questions that nobody can answer any more to the task's notify
// target (the trigger's, by default the org's admins), and returns how many questions it changed. A
// recipient counts only while they can answer (not deleted, role member or higher); when the last one
// leaves the org or becomes a viewer, the question would otherwise wait on nobody until it expires.
// The target resolves as it does when a question is asked: a notify person who can no longer answer
// falls back to the default (recorded as requestedTarget and fallbackReason); a role nobody holds
// gets no recipients and the question is left alone. The new recipients get the question.asked
// notification and their age starts then (ADR 0065 amendment 2026-10-28). Who may answer is an
// authorization change, so each question that changes is audited as question.retarget, by the system,
// in the same transaction. An org with nobody to give the question to changes nothing, and the
// question expires as before.
func RetargetOrphaned(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	orgs, err := store.New(pool).ListOrgsWithOpenQuestions(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	for _, org := range orgs {
		n := 0
		_, err := audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
			orphans, err := q.ListOrphanedQuestions(ctx, org)
			if err != nil {
				return err
			}
			n = 0
			for _, o := range orphans {
				target := TaskTarget(o.NotifyKind, o.NotifyUserID, o.NotifyRole)
				people, err := Recipients(ctx, q, org, target)
				if err != nil {
					return err
				}
				var fallback map[string]any
				if target.Kind == store.NotifyTargetKindUser && len(people) == 0 {
					// As on the ask path: only a notify person who can no longer answer falls back to
					// the default. A role nobody holds stays empty: the question is not widened to
					// the admins, and (as ask_user refuses it) stays as it is until it expires.
					fallback = map[string]any{
						"requestedTarget": map[string]any{"kind": "user", "user": target.User.String()},
						"fallbackReason":  "the trigger's notify person is no longer a member who can answer",
					}
					target = DefaultTarget
					if people, err = Recipients(ctx, q, org, target); err != nil {
						return err
					}
				}
				if len(people) == 0 {
					continue
				}
				added, err := q.AddWaitingQuestionRecipients(ctx, store.AddWaitingQuestionRecipientsParams{OrgID: org, QuestionID: o.ID, UserIds: people})
				if err != nil {
					return err
				}
				if len(added) == 0 {
					continue
				}
				qn, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: org, ID: o.ID})
				if err != nil {
					return err
				}
				body, _ := json.Marshal(map[string]any{
					"question_id": qn.ID.String(), "task_id": qn.TaskID.String(), "question": qn.Question,
				})
				ids := make([]string, 0, len(added))
				for _, u := range added {
					ids = append(ids, u.String())
					if _, err := q.InsertNotification(ctx, store.InsertNotificationParams{
						OrgID: org, UserID: u, Kind: NotificationKind, DedupeKey: "question:" + qn.ID.String(), Payload: body,
					}); err != nil {
						return err
					}
				}
				name := ""
				if target.Kind == store.NotifyTargetKindUser {
					if name, err = q.GetUserDisplayName(ctx, target.User); err != nil {
						return err
					}
				}
				toLabel := Label(target, name)
				payload := map[string]any{
					"question_id": qn.ID.String(), "to_label": toLabel, "recipients": ids,
					"reason": "nobody it was addressed to can answer any more",
				}
				after := map[string]any{
					"toLabel": toLabel, "recipients": ids, "reason": "nobody it was addressed to can answer any more",
					"taskId": qn.TaskID.String(), "runId": qn.RunID.String(), "agentId": o.AgentID.String(),
				}
				for k, v := range fallback {
					payload[k], after[k] = v, v
				}
				if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
					OrgID: org, TaskID: qn.TaskID, RunID: qn.RunID, Kind: KindRetargeted, ActorKind: store.TaskActorKindSystem,
					Payload: mustJSON(payload),
				}); err != nil {
					return err
				}
				b.Org(audit.Event{
					Org: org, Actor: audit.System("koinon-api:question-timers"), Action: "question.retarget", TargetKind: "question", TargetID: qn.ID.String(),
					After: after,
				})
				n++
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("retarget questions of org %s: %w", org, err))
			continue
		}
		total += n
	}
	return total, errors.Join(errs...)
}
