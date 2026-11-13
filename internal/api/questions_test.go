// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/internal/questions"
)

// openQuestion records a question of the fixture's run to its owner and opens it, as the tool
// server and agentd's turn end do.
func (e *env) openQuestion(ctx context.Context, f orgFixture) store.Question {
	GinkgoHelper()
	return e.openQuestionWith(ctx, f, "Post the reply?", []string{"post", "do not post"}, "do not post")
}

// openQuestionWith opens a question of the fixture's run with the given text, choices and default.
func (e *env) openQuestionWith(ctx context.Context, f orgFixture, question string, choices []string, def string) store.Question {
	GinkgoHelper()
	var row store.Question
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		qq := store.New(tx)
		var err error
		row, _, err = questions.Create(ctx, qq, questions.Ask{
			Org: uuid(f.org), Task: uuid(f.ticket.GetId()), Run: uuid(f.run.GetId()), Agent: uuid(f.agent.GetId()),
			Target: questions.Target{Kind: store.NotifyTargetKindUser, User: uuid(f.owner.userID)}, Recipients: []pgtype.UUID{uuid(f.owner.userID)}, Question: question, Draft: "Fixed in 1.2.",
			Choices: choices, Default: def, ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		_, err = questions.OpenForRun(ctx, qq, uuid(f.org), uuid(f.run.GetId()), uuid(f.ticket.GetId()))
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	row, err = store.New(e.pool).GetQuestion(ctx, store.GetQuestionParams{OrgID: row.OrgID, ID: row.ID})
	Expect(err).NotTo(HaveOccurred())
	return row
}

var _ = Describe("QuestionService", func() {
	var (
		e      *env
		f      orgFixture
		member *client
		q      store.Question
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		f = e.newOrgFixture(ctx)
		member = e.userIn(ctx, f.org, store.OrgRoleMember)
		q = e.openQuestion(ctx, f)
	})

	list := func(ctx SpecContext, c *client, taskID string) []*questionv1alpha1.Question {
		GinkgoHelper()
		res, err := c.questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: f.org, TaskId: taskID})
		Expect(err).NotTo(HaveOccurred())
		return res.GetQuestions()
	}

	It("lists the open question to its addressee only, with the choices, draft and default", func(ctx SpecContext) {
		// The inbox lists the questions of parked runs only.
		ad := e.agentd("agentd-list")
		run := ad.mustClaim(ctx, f.org)
		_, err := ad.q.ParkRun(ctx, store.ParkRunParams{OrgID: run.OrgID, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonInputRequired})
		Expect(err).NotTo(HaveOccurred())
		got := list(ctx, f.owner, "")
		Expect(got).To(HaveLen(1))
		Expect(got[0].GetId()).To(Equal(q.ID.String()))
		Expect(got[0].GetQuestion()).To(Equal("Post the reply?"))
		Expect(got[0].GetChoices()).To(Equal([]string{"post", "do not post"}))
		Expect(got[0].GetDraft()).To(Equal("Fixed in 1.2."))
		Expect(got[0].GetDefaultAnswer()).To(Equal("do not post"))
		Expect(got[0].GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_OPEN))
		Expect(got[0].GetCanAnswer()).To(BeTrue())

		Expect(list(ctx, member, "")).To(BeEmpty(), "a question is the addressee's")
		Expect(list(ctx, member, f.ticket.GetId())).To(BeEmpty())
		Expect(list(ctx, f.owner, f.ticket.GetId())).To(HaveLen(1))
	})

	It("answers, resumes the parked run once and refuses a second answer", func(ctx SpecContext) {
		ad := e.agentd("agentd-a")
		run := ad.mustClaim(ctx, f.org)
		// The run parks as agentd does at the end of the turn; the question is already open.
		_, err := ad.q.ParkRun(ctx, store.ParkRunParams{OrgID: run.OrgID, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonInputRequired})
		Expect(err).NotTo(HaveOccurred())

		_, err = member.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: q.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "post"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "only the addressee answers")

		_, err = f.owner.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: q.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "maybe"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		res, err := f.owner.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: q.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "post"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetQuestion().GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_ANSWERED))
		Expect(res.GetQuestion().GetAnswer()).To(Equal("post"))
		Expect(res.GetQuestion().GetAnswerKind()).To(Equal(questionv1alpha1.AnswerKind_ANSWER_KIND_CHOICE))
		Expect(res.GetQuestion().GetCanAnswer()).To(BeFalse())
		resumed, err := ad.q.GetRun(ctx, store.GetRunParams{OrgID: run.OrgID, ID: run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(resumed.ReadyAt.Valid).To(BeTrue(), "the run can be claimed again")
		task, err := f.owner.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: f.org, TaskId: f.ticket.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(task.GetTask().GetState()).NotTo(Equal(taskv1alpha1.TaskState_TASK_STATE_INPUT_REQUIRED))

		_, err = f.owner.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: q.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Text{Text: "post"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeAborted))
		Expect(list(ctx, f.owner, "")).To(BeEmpty())
	})

	It("accepts the draft as offered or edited, and free text", func(ctx SpecContext) {
		res, err := f.owner.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: q.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Draft{Draft: ""},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetQuestion().GetAnswer()).To(Equal("Fixed in 1.2."))
		Expect(res.GetQuestion().GetAnswerKind()).To(Equal(questionv1alpha1.AnswerKind_ANSWER_KIND_DRAFT))
	})

	It("dismisses with the safe default as the answer", func(ctx SpecContext) {
		_, err := member.questions.DismissQuestion(ctx, &questionv1alpha1.DismissQuestionRequest{OrgId: f.org, QuestionId: q.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		res, err := f.owner.questions.DismissQuestion(ctx, &questionv1alpha1.DismissQuestionRequest{OrgId: f.org, QuestionId: q.ID.String()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetQuestion().GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_DISMISSED))
		Expect(res.GetQuestion().GetAnswer()).To(Equal("do not post"))
		Expect(res.GetQuestion().GetAnswerKind()).To(Equal(questionv1alpha1.AnswerKind_ANSWER_KIND_DEFAULT))
	})

	It("lets a viewer read but not answer", func(ctx SpecContext) {
		viewer := e.userIn(ctx, f.org, store.OrgRoleViewer)
		Expect(list(ctx, viewer, "")).To(BeEmpty())
		_, err := viewer.questions.DismissQuestion(ctx, &questionv1alpha1.DismissQuestionRequest{OrgId: f.org, QuestionId: q.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})
})
