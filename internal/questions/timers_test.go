// SPDX-License-Identifier: Apache-2.0

package questions_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/questions"
)

var _ = Describe("reminders and expiry (TK4)", func() {
	var (
		w     world
		base  time.Time
		row   store.Question
		sends durable.Sender
	)
	// restart is what a restart of koinon-api is for the timers: a new notifier with its handlers
	// and a new sweep, over the same database.
	restart := func() durable.Sender {
		n := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(n, pool)
		return n
	}
	run := func(ctx context.Context, send durable.Sender, at time.Time) (questions.SweepResult, error) {
		return questions.RunOrgTimers(ctx, pool, send, w.org, at)
	}
	count := func(ctx context.Context, kind string) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = $2 AND kind = $3`, w.org, w.user, kind).Scan(&n)).To(Succeed())
		return n
	}
	events := func(ctx context.Context, kind string) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND task_id = $2 AND kind = $3`, w.org, w.task, kind).Scan(&n)).To(Succeed())
		return n
	}
	ready := func(ctx context.Context) bool {
		GinkgoHelper()
		r, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())
		return r.ReadyAt.Valid
	}

	BeforeEach(func(ctx SpecContext) {
		w = newWorld(ctx)
		base = time.Now()
		sends = restart()
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) (err error) {
			row, _, err = questions.Create(ctx, q, questions.Ask{
				Org: w.org, Task: w.task, Run: w.run.ID, Agent: w.agent,
				Target: questions.Target{Kind: store.NotifyTargetKindUser, User: w.user}, Recipients: []pgtype.UUID{w.user},
				Question: "Post the reply?", Choices: []string{"post", "do not post"}, Default: "do not post",
				ExpiresAt: base.Add(questions.DefaultTTL), RemindAt: base.Add(questions.DefaultReminder),
			})
			if err != nil {
				return err
			}
			if _, err = questions.OpenForRun(ctx, q, w.org, w.run.ID, w.task); err != nil {
				return err
			}
			_, err = q.ParkRun(ctx, store.ParkRunParams{
				OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, ParkReason: store.RunParkReasonInputRequired,
				ParkExpiresAt: pgtype.Timestamptz{Time: row.ExpiresAt.Time, Valid: true},
			})
			return err
		})).To(Succeed())
	})

	It("sends nothing before the reminder is due", func(ctx SpecContext) {
		res, err := run(ctx, sends, base.Add(questions.DefaultReminder-time.Minute))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{}))
		Expect(count(ctx, questions.ReminderNotificationKind)).To(BeZero())
	})

	It("reminds the person asked once after 24 hours, also across a restart", func(ctx SpecContext) {
		at := base.Add(questions.DefaultReminder + time.Minute)
		res, err := run(ctx, sends, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{Sent: 1}))
		Expect(count(ctx, questions.ReminderNotificationKind)).To(Equal(1))
		var msg string
		Expect(pool.QueryRow(ctx, `SELECT payload->>'message' FROM notifications WHERE org_id = $1 AND kind = $2`, w.org, questions.ReminderNotificationKind).Scan(&msg)).To(Succeed())
		Expect(msg).To(ContainSubstring("Post the reply?"))
		Expect(msg).To(ContainSubstring(`the default (do not post) applies`))

		for _, s := range []durable.Sender{sends, restart()} {
			res, err = run(ctx, s, at.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(questions.SweepResult{}))
		}
		Expect(count(ctx, questions.ReminderNotificationKind)).To(Equal(1))
		got, _ := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: row.ID})
		Expect(got.RemindedAt.Valid).To(BeTrue())
		Expect(got.State).To(Equal(store.QuestionStateOpen), "a reminder does not resolve the question")
		Expect(ready(ctx)).To(BeFalse())
	})

	It("does not remind about a question that was answered meanwhile", func(ctx SpecContext) {
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})).To(Succeed())
		res, err := run(ctx, sends, base.Add(2*questions.DefaultReminder))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{}))
		Expect(count(ctx, questions.ReminderNotificationKind)).To(BeZero())
	})

	It("expires after 7 days: the default applies and the run resumes once, also across a restart", func(ctx SpecContext) {
		By("a sweep before the expiry changes nothing but the reminder")
		res, err := run(ctx, sends, base.Add(questions.DefaultTTL-time.Minute))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{Sent: 1}))
		Expect(ready(ctx)).To(BeFalse())

		at := base.Add(questions.DefaultTTL + time.Minute)
		res, err = run(ctx, sends, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(questions.SweepResult{Expired: 1}))
		got, _ := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: row.ID})
		Expect(got.State).To(Equal(store.QuestionStateExpired))
		Expect(got.Answer.String).To(Equal("do not post"))
		Expect(got.AnswerKind.QuestionAnswerKind).To(Equal(store.QuestionAnswerKindDefault))
		Expect(got.AnsweredBy.Valid).To(BeFalse())
		Expect(ready(ctx)).To(BeTrue(), "the parked run is resumed: the one no-answer run")

		By("sweeping again, as a restarted API or a second replica would, does nothing")
		for _, s := range []durable.Sender{sends, restart()} {
			res, err = run(ctx, s, at.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(questions.SweepResult{}))
		}
		Expect(events(ctx, questions.KindExpired)).To(Equal(1))

		By("a late answer is refused")
		err = tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotOpen))
	})

	It("expires concurrent sweeps once", func(ctx SpecContext) {
		at := base.Add(questions.DefaultTTL + time.Minute)
		results := make(chan questions.SweepResult, 4)
		for range 4 {
			go func() {
				defer GinkgoRecover()
				res, err := run(ctx, restart(), at)
				Expect(err).NotTo(HaveOccurred())
				results <- res
			}()
		}
		total := 0
		for range 4 {
			total += (<-results).Expired
		}
		Expect(total).To(Equal(1))
		Expect(events(ctx, questions.KindExpired)).To(Equal(1))
	})

	It("refuses a reminder that is not before the expiry", func() {
		a := questions.Ask{Recipients: []pgtype.UUID{{Valid: true}}, Question: "q", Default: "d", ExpiresAt: base.Add(time.Hour), RemindAt: base.Add(time.Hour)}
		Expect(a.Validate()).To(MatchError(ContainSubstring("before the expiry")))
	})
})
