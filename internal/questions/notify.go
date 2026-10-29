// SPDX-License-Identifier: Apache-2.0

package questions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

// NotifyAsked is the notification kind that tells a person a question waits for them (ADR 0065
// decision 8). Today it writes the in-app notification; chat messages (Slack, Teams) join it with
// the chat connectors (v0.2). The inbox itself needs no notification: it lists the open question.
const NotifyAsked = "question.asked"

// NotificationKind is the kind of the in-app notification row.
const NotificationKind = "question.asked"

type payload struct {
	Org      string `json:"org"`
	Question string `json:"question"`
}

// notification is the notification for an opened question. Its ID is derived from the question, so
// sending it twice is one workflow (practice B2).
func notification(row store.Question) durable.Notification {
	p, _ := json.Marshal(payload{Org: row.OrgID.String(), Question: row.ID.String()})
	return durable.Notification{ID: NotifyAsked + ":" + row.ID.String(), Kind: NotifyAsked, Payload: string(p)}
}

// RegisterNotifications registers the handler of the question notification on n. Call it before the
// DBOS runtime launches, so a workflow recovered at start finds it.
func RegisterNotifications(n *durable.Notifier, pool *pgxpool.Pool) {
	registerReminder(n, pool)
	n.Handle(NotifyAsked, func(ctx context.Context, note durable.Notification) error {
		err := deliver(ctx, pool, note)
		if err == nil {
			return nil
		}
		// Each attempt of the DBOS step (and each inline sweep) lands here. After giveUpAfter the
		// question counts as notified, so the sweep stops re-sending a workflow that failed for
		// good (its fixed ID returns the recorded failure) and the inbox still lists the question.
		var p payload
		if json.Unmarshal([]byte(note.Payload), &p) == nil {
			var org, id pgtype.UUID
			if org.Scan(p.Org) == nil && id.Scan(p.Question) == nil {
				row, rerr := store.New(pool).RecordQuestionNotifyFailure(ctx, store.RecordQuestionNotifyFailureParams{OrgID: org, ID: id, GiveUp: giveUpAfter})
				if rerr == nil && row.NotifiedAt.Valid {
					slog.Error("question notification abandoned after repeated failures; the question stays in the inbox",
						"org", p.Org, "question", p.Question, "failures", row.NotifyFailures, "err", err)
				}
			}
		}
		return err
	})
}

// giveUpAfter is how many failed deliveries a question's notification gets before it is abandoned.
const giveUpAfter = 8

func deliver(ctx context.Context, pool *pgxpool.Pool, note durable.Notification) error {
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
	// A question answered before this ran needs no announcement.
	if row.State == store.QuestionStateOpen {
		body, _ := json.Marshal(map[string]any{
			"question_id": row.ID.String(), "task_id": row.TaskID.String(), "question": row.Question,
		})
		if err := notifyRecipients(ctx, q, row, NotificationKind, "question:"+row.ID.String(), body); err != nil {
			return err
		}
	}
	_, err = q.MarkQuestionNotified(ctx, store.MarkQuestionNotifiedParams{OrgID: org, ID: id})
	return err
}

// notifyRecipients writes the in-app notification for each recipient who can still answer. The
// insert is idempotent per user (the dedupe key), so a retry adds nobody twice.
func notifyRecipients(ctx context.Context, q *store.Queries, row store.Question, kind, dedupe string, body []byte) error {
	users, err := q.ListActiveQuestionRecipientIDs(ctx, store.ListActiveQuestionRecipientIDsParams{OrgID: row.OrgID, QuestionID: row.ID})
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := q.InsertNotification(ctx, store.InsertNotificationParams{
			OrgID: row.OrgID, UserID: u, Kind: kind, DedupeKey: dedupe, Payload: body,
		}); err != nil {
			return err
		}
	}
	return nil
}

// NotifyPending sends the notification of every opened question that has none yet. The park that
// opens a question commits in koinon-agentd, which holds no notifier; koinon-api calls this on a
// timer, so the notification follows the commit by at most one interval and survives a crash of
// either process. It returns how many it sent.
func NotifyPending(ctx context.Context, pool *pgxpool.Pool, send durable.Sender) (int, error) {
	q := store.New(pool)
	orgs, err := q.ListOrgsWithQuestionsToNotify(ctx)
	if err != nil {
		return 0, err
	}
	sent := 0
	// One question or org that keeps failing must not block the others: log each failure, go on and
	// return them joined.
	var errs []error
	for _, org := range orgs {
		rows, err := q.ListQuestionsToNotify(ctx, store.ListQuestionsToNotifyParams{OrgID: org, MaxRows: 100})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range rows {
			if err := send.Send(ctx, notification(row)); err != nil {
				slog.Error("question notification failed", "org", org.String(), "question", row.ID.String(), "err", err)
				errs = append(errs, fmt.Errorf("question %s: %w", row.ID, err))
				continue
			}
			sent++
		}
	}
	return sent, errors.Join(errs...)
}

// Sweeper calls NotifyPending, RetargetOrphaned, RunTimers and CancelOfFinishedRuns every interval until ctx ends.
func Sweeper(ctx context.Context, pool *pgxpool.Pool, send durable.Sender, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := NotifyPending(ctx, pool, send); err != nil && ctx.Err() == nil {
			log.Error("question notification sweep failed", "err", err)
		}
		if n, err := RetargetOrphaned(ctx, pool); err != nil && ctx.Err() == nil {
			log.Error("giving orphaned questions to the org's admins failed", "err", err)
		} else if n > 0 {
			log.Info("gave orphaned questions to the org's admins", "questions", n)
		}
		if res, err := RunTimers(ctx, pool, send, time.Now()); err != nil && ctx.Err() == nil {
			log.Error("question reminder and expiry sweep failed", "err", err)
		} else if res != (SweepResult{}) {
			log.Info("question timers", "reminders_sent", res.Sent, "expired", res.Expired)
		}
		if _, err := CancelOfFinishedRuns(ctx, pool); err != nil && ctx.Err() == nil {
			log.Error("canceling the questions of finished runs failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
