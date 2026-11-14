// SPDX-License-Identifier: Apache-2.0

package questions_test

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/questions"
)

func tx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, pool, fn)
	return err
}

var _ = Describe("Ask.Validate", func() {
	base := func() questions.Ask {
		return questions.Ask{Recipients: []pgtype.UUID{{Valid: true}}, Question: "Post the reply?", Default: "do not post", Choices: []string{"post", "do not post"}}
	}
	It("accepts a question with choices and a default among them", func() {
		a := base()
		Expect(a.Validate()).To(Succeed())
	})
	It("requires a safe default (ADR 0065 decision 10a)", func() {
		a := base()
		a.Default = " "
		Expect(a.Validate()).To(MatchError(ContainSubstring("default is required")))
	})
	It("requires the default to be one of the choices", func() {
		a := base()
		a.Default = "post it anyway"
		Expect(a.Validate()).To(MatchError(ContainSubstring("one of the choices")))
	})
	It("takes any default with free text", func() {
		a := questions.Ask{Recipients: []pgtype.UUID{{Valid: true}}, Question: "What should the title be?", Default: "keep the current title"}
		Expect(a.Validate()).To(Succeed())
	})
	It("refuses empty, duplicate and too many choices, and long text", func() {
		a := base()
		a.Choices = []string{"a", "a"}
		a.Default = "a"
		Expect(a.Validate()).To(MatchError(ContainSubstring("listed twice")))
		a = base()
		a.Choices = make([]string, 11)
		Expect(a.Validate()).To(MatchError(ContainSubstring("at most 10")))
		a = base()
		a.Question = strings.Repeat("x", questions.MaxQuestionBytes+1)
		Expect(a.Validate()).To(MatchError(ContainSubstring("at most")))
	})
})

var _ = Describe("a question", func() {
	var (
		w   world
		ask questions.Ask
	)
	BeforeEach(func(ctx SpecContext) {
		w = newWorld(ctx)
		ask = questions.Ask{
			Org: w.org, Task: w.task, Run: w.run.ID, Agent: w.agent,
			Target: questions.Target{Kind: store.NotifyTargetKindUser, User: w.user}, Recipients: []pgtype.UUID{w.user},
			Question: "Post the reply?", Draft: "Thanks, fixed in 1.2.", Choices: []string{"post", "do not post"}, Default: "do not post",
		}
	})
	create := func(ctx context.Context, a questions.Ask) (row store.Question, created bool) {
		GinkgoHelper()
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) (err error) {
			row, created, err = questions.Create(ctx, q, a)
			return err
		})).To(Succeed())
		return row, created
	}
	open := func(ctx context.Context) {
		GinkgoHelper()
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.OpenForRun(ctx, q, w.org, w.run.ID, w.task)
			return err
		})).To(Succeed())
	}

	park := func(ctx context.Context) {
		GinkgoHelper()
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := q.ParkRun(ctx, store.ParkRunParams{
				OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, ParkReason: store.RunParkReasonInputRequired,
				ParkExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
			})
			return err
		})).To(Succeed())
	}

	It("is the same question when asked twice in one run", func(ctx SpecContext) {
		first, created := create(ctx, ask)
		Expect(created).To(BeTrue())
		again, created := create(ctx, ask)
		Expect(created).To(BeFalse())
		Expect(again.ID).To(Equal(first.ID))
		ask.Question = "Post the reply now?"
		other, created := create(ctx, ask)
		Expect(created).To(BeTrue())
		Expect(other.ID).NotTo(Equal(first.ID))
	})

	It("can only be addressed to a member of the org", func(ctx SpecContext) {
		stranger := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}
		ask.Target.User, ask.Recipients = stranger, []pgtype.UUID{stranger}
		err := tx(ctx, func(q *store.Queries, b audit.Batch) error { _, _, err := questions.Create(ctx, q, ask); return err })
		Expect(err).To(MatchError(questions.ErrNotMember))
	})

	It("is not answerable until it is opened, and open questions show in the addressee's inbox only", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		err := tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotOpen))
		inbox, _ := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: w.user})
		Expect(inbox).To(BeEmpty())

		open(ctx)
		park(ctx)
		inbox, _ = q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: w.user})
		Expect(inbox).To(HaveLen(1))
		bob := w.member(ctx)
		inbox, _ = q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: bob})
		Expect(inbox).To(BeEmpty())
		err = tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, row.ID, bob, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotAskedTo))
	})

	DescribeTable("checks the answer against the question",
		func(ctx SpecContext, ans questions.Answer, want string) {
			row, _ := create(ctx, ask)
			open(ctx)
			err := tx(ctx, func(q *store.Queries, b audit.Batch) error {
				_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, ans)
				return err
			})
			if want == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(MatchError(questions.ErrInvalid))
		},
		Entry("a listed choice", questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"}, ""),
		Entry("a choice that is not listed", questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "maybe"}, "not one of the choices"),
		Entry("the draft as offered", questions.Answer{Kind: store.QuestionAnswerKindDraft}, ""),
		Entry("the draft, edited", questions.Answer{Kind: store.QuestionAnswerKindDraft, Text: "Thanks!"}, ""),
		Entry("free text", questions.Answer{Kind: store.QuestionAnswerKindText, Text: "post, but friendlier"}, ""),
		Entry("empty text", questions.Answer{Kind: store.QuestionAnswerKindText, Text: "  "}, "empty"),
		Entry("the default is not an answer a person gives", questions.Answer{Kind: store.QuestionAnswerKindDefault, Text: "x"}, "choice, the draft or text"),
	)

	It("resolves the park once, only when no approval or other question is open", func(ctx SpecContext) {
		first, _ := create(ctx, ask)
		ask.Question = "And the title?"
		ask.Choices, ask.Draft, ask.Default = nil, "", "keep it"
		second, _ := create(ctx, ask)
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			if _, err := questions.OpenForRun(ctx, q, w.org, w.run.ID, w.task); err != nil {
				return err
			}
			_, err := q.ParkRun(ctx, store.ParkRunParams{
				OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, ParkReason: store.RunParkReasonInputRequired,
				ParkExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
			})
			return err
		})).To(Succeed())
		ready := func() bool {
			r, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: w.run.ID})
			Expect(err).NotTo(HaveOccurred())
			return r.ReadyAt.Valid
		}

		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, first.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})).To(Succeed())
		Expect(ready()).To(BeFalse(), "another question is still open")
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Dismiss(ctx, q, b, w.org, second.ID, w.user)
			return err
		})).To(Succeed())
		Expect(ready()).To(BeTrue())

		By("the dismissed question's answer is the default, and delivery is tracked")
		got, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: second.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Answer.String).To(Equal("keep it"))
		Expect(got.AnswerKind.QuestionAnswerKind).To(Equal(store.QuestionAnswerKindDefault))
		Expect(got.AnsweredBy).To(Equal(w.user))
		undelivered, err := q.ListUndeliveredQuestionsForRun(ctx, store.ListUndeliveredQuestionsForRunParams{OrgID: w.org, RunID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(undelivered).To(HaveLen(2))
	})

	It("answers an already answered question with ErrNotOpen while a turn end holds the run's lock", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		open(ctx)
		park(ctx)
		answer := func(ctx context.Context) error {
			return tx(ctx, func(q *store.Queries, b audit.Batch) error {
				_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
				return err
			})
		}
		Expect(answer(ctx)).To(Succeed())

		By("a turn end in progress: the run is locked and the answered question is marked delivered")
		turn, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = turn.Rollback(context.WithoutCancel(ctx)) }()
		tq := store.New(turn)
		_, err = tq.MarkQuestionsDelivered(ctx, store.MarkQuestionsDeliveredParams{OrgID: w.org, Ids: []pgtype.UUID{row.ID}})
		Expect(err).NotTo(HaveOccurred())
		_, err = tq.LockRun(ctx, store.LockRunParams{OrgID: w.org, ID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())

		By("the second answer is refused at once, without waiting for the turn end")
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		Expect(answer(waitCtx)).To(MatchError(questions.ErrNotOpen))
		Expect(turn.Commit(ctx)).To(Succeed())
	})

	It("is refused, not deadlocked, when an answer commits after the pre-check and a turn end has locked the answered row", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		open(ctx)
		park(ctx)

		By("a holder keeps the run's lock")
		holder, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
		_, err = store.New(holder).LockRun(ctx, store.LockRunParams{OrgID: w.org, ID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())
		var holderPID int32
		Expect(holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID)).To(Succeed())
		// Sessions that wait for a lock held by the holder, or queued behind such a waiter.
		waiting := func() int {
			var n int
			Expect(pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE cardinality(pg_blocking_pids(pid)) > 0 AND pid <> $1", holderPID).Scan(&n)).To(Succeed())
			return n
		}

		By("answer B passes the pre-check (the question is open) and waits for the run's lock")
		second := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			second <- tx(ctx, func(q *store.Queries, b audit.Batch) error {
				_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
				return err
			})
		}()
		Eventually(waiting).WithTimeout(10 * time.Second).Should(Equal(1))

		By("answer A is committed, and a turn end locks the answered row, then waits for the run's lock")
		_, err = pool.Exec(ctx, `UPDATE questions SET state = 'answered', answer = 'post', answer_kind = 'choice', resolved_at = now()
			WHERE org_id = $1 AND id = $2`, w.org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		turnDone := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			turn, err := pool.Begin(ctx)
			if err != nil {
				turnDone <- err
				return
			}
			defer func() { _ = turn.Rollback(context.WithoutCancel(ctx)) }()
			tq := store.New(turn)
			if _, err = tq.MarkQuestionsDelivered(ctx, store.MarkQuestionsDeliveredParams{OrgID: w.org, Ids: []pgtype.UUID{row.ID}}); err == nil {
				_, err = tq.LockRun(ctx, store.LockRunParams{OrgID: w.org, ID: w.run.ID})
			}
			if err == nil {
				err = turn.Commit(ctx)
			}
			turnDone <- err
		}()
		Eventually(waiting).WithTimeout(10 * time.Second).Should(Equal(2))

		By("when the holder ends, B re-reads the question under the run's lock and is refused")
		Expect(holder.Commit(ctx)).To(Succeed())
		Eventually(second).WithTimeout(10 * time.Second).Should(Receive(MatchError(questions.ErrNotOpen)))
		Eventually(turnDone).WithTimeout(10 * time.Second).Should(Receive(BeNil()))
	})

	It("cannot be addressed to a viewer, who cannot answer", func(ctx SpecContext) {
		viewer := w.member(ctx)
		_, err := pool.Exec(ctx, `UPDATE org_memberships SET role = 'viewer' WHERE org_id = $1 AND user_id = $2`, w.org, viewer)
		Expect(err).NotTo(HaveOccurred())
		ask.Target.User, ask.Recipients = viewer, []pgtype.UUID{viewer}
		err = tx(ctx, func(q *store.Queries, b audit.Batch) error { _, _, err := questions.Create(ctx, q, ask); return err })
		Expect(err).To(MatchError(questions.ErrNotMember))
	})

	It("is canceled with its run: it leaves the inbox and can no longer be answered", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		ask.Question = "Never opened?"
		asked, _ := create(ctx, ask)
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.OpenForRun(ctx, q, w.org, w.run.ID, w.task)
			return err
		})).To(Succeed())
		park(ctx)
		inbox, _ := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: w.user})
		Expect(inbox).To(HaveLen(2))

		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled by user"})
		Expect(err).NotTo(HaveOccurred())
		inbox, _ = q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: w.org, UserID: w.user})
		Expect(inbox).To(BeEmpty(), "hidden at once, before the sweep")
		err = tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Respond(ctx, q, b, w.org, row.ID, w.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "post"})
			return err
		})
		Expect(err).To(MatchError(questions.ErrNotOpen))

		n, err := questions.CancelOfFinishedRuns(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2))
		for _, id := range []pgtype.UUID{row.ID, asked.ID} {
			got, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(store.QuestionStateCanceled))
			Expect(got.ResolvedAt.Valid).To(BeTrue())
		}
		evs, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: w.org, TaskID: w.task, MaxEvents: 100})
		Expect(err).NotTo(HaveOccurred())
		var kinds []string
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		Expect(kinds).To(ContainElement(questions.KindCanceled))
		n, err = questions.CancelOfFinishedRuns(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "a second sweep finds nothing")
	})

	It("keeps notifying the other questions when one keeps failing, and gives up on the failing one", func(ctx SpecContext) {
		bad, _ := create(ctx, ask)
		ask.Question = "Another?"
		good, _ := create(ctx, ask)
		open(ctx)
		n := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(n, pool)
		send := senderFunc(func(ctx context.Context, note durable.Notification) error {
			if note.ID == questions.NotifyAsked+":"+bad.ID.String() {
				return n.Send(ctx, durable.Notification{ID: note.ID, Kind: "no.such.kind"})
			}
			return n.Send(ctx, note)
		})
		sent, err := questions.NotifyPending(ctx, pool, send)
		Expect(err).To(HaveOccurred())
		Expect(sent).To(BeNumerically(">=", 1), "the others are still sent")
		got, _ := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: good.ID})
		Expect(got.NotifiedAt.Valid).To(BeTrue())
		got, _ = q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: bad.ID})
		Expect(got.NotifiedAt.Valid).To(BeFalse())
	})

	It("expires without an actor and keeps the default as the answer", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		open(ctx)
		Expect(tx(ctx, func(q *store.Queries, b audit.Batch) error {
			_, err := questions.Expire(ctx, q, b, w.org, row.ID)
			return err
		})).To(Succeed())
		got, _ := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: row.ID})
		Expect(got.State).To(Equal(store.QuestionStateExpired))
		Expect(got.AnsweredBy.Valid).To(BeFalse())
		Expect(got.Answer.String).To(Equal("do not post"))
	})

	It("lists the open questions past their expiry for TK4", func(ctx SpecContext) {
		ask.ExpiresAt = time.Now().Add(-time.Minute)
		row, _ := create(ctx, ask)
		open(ctx)
		due, err := q.ListDueQuestions(ctx, store.ListDueQuestionsParams{OrgID: w.org, Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(due).To(HaveLen(1))
		Expect(due[0].ID).To(Equal(row.ID))
	})

	It("notifies once, in the inbox's notification list, after it is opened", func(ctx SpecContext) {
		row, _ := create(ctx, ask)
		n := durable.NewNotifier(nil, nil)
		questions.RegisterNotifications(n, pool)

		notes := func() []store.Notification {
			GinkgoHelper()
			n, err := q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: w.org, UserID: w.user, MaxRows: 10})
			Expect(err).NotTo(HaveOccurred())
			return n
		}
		_, err := questions.NotifyPending(ctx, pool, n)
		Expect(err).NotTo(HaveOccurred())
		Expect(notes()).To(BeEmpty(), "an asked question is not announced")

		open(ctx)
		_, err = questions.NotifyPending(ctx, pool, n)
		Expect(err).NotTo(HaveOccurred())
		ns := notes()
		Expect(ns).To(HaveLen(1))
		Expect(ns[0].Kind).To(Equal(questions.NotificationKind))
		Expect(ns[0].DedupeKey).To(Equal("question:" + row.ID.String()))

		_, err = questions.NotifyPending(ctx, pool, n)
		Expect(err).NotTo(HaveOccurred())
		Expect(notes()).To(HaveLen(1), "it is marked notified, so it is not sent again")
		got, err := q.GetQuestion(ctx, store.GetQuestionParams{OrgID: w.org, ID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.NotifiedAt.Valid).To(BeTrue())
	})
})

type senderFunc func(ctx context.Context, n durable.Notification) error

func (f senderFunc) Send(ctx context.Context, n durable.Notification) error { return f(ctx, n) }
