// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1/questionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/questions"
)

var _ questionv1alpha1connect.QuestionServiceHandler = (*Server)(nil)

var questionStateToProto = map[store.QuestionState]questionv1alpha1.QuestionState{
	store.QuestionStateAsked:     questionv1alpha1.QuestionState_QUESTION_STATE_ASKED,
	store.QuestionStateOpen:      questionv1alpha1.QuestionState_QUESTION_STATE_OPEN,
	store.QuestionStateAnswered:  questionv1alpha1.QuestionState_QUESTION_STATE_ANSWERED,
	store.QuestionStateDismissed: questionv1alpha1.QuestionState_QUESTION_STATE_DISMISSED,
	store.QuestionStateExpired:   questionv1alpha1.QuestionState_QUESTION_STATE_EXPIRED,
	store.QuestionStateCanceled:  questionv1alpha1.QuestionState_QUESTION_STATE_CANCELED,
}

var answerKindToProto = map[store.QuestionAnswerKind]questionv1alpha1.AnswerKind{
	store.QuestionAnswerKindChoice:  questionv1alpha1.AnswerKind_ANSWER_KIND_CHOICE,
	store.QuestionAnswerKindDraft:   questionv1alpha1.AnswerKind_ANSWER_KIND_DRAFT,
	store.QuestionAnswerKindText:    questionv1alpha1.AnswerKind_ANSWER_KIND_TEXT,
	store.QuestionAnswerKindDefault: questionv1alpha1.AnswerKind_ANSWER_KIND_DEFAULT,
}

func questionProto(row store.Question, c caller) *questionv1alpha1.Question {
	return &questionv1alpha1.Question{
		Id: idString(row.ID), OrgId: idString(row.OrgID), TaskId: idString(row.TaskID), RunId: idString(row.RunID),
		AgentId: idString(row.AgentID), AskedTo: idString(row.AskedTo), Question: row.Question,
		Choices: questions.ChoicesOf(row), Draft: row.Draft.String, DefaultAnswer: row.DefaultAnswer,
		State: questionStateToProto[row.State], Answer: row.Answer.String,
		AnswerKind: answerKindToProto[row.AnswerKind.QuestionAnswerKind], AnsweredBy: idString(row.AnsweredBy),
		ExpiresAt: tsProto(row.ExpiresAt), OpenedAt: tsProto(row.OpenedAt), ResolvedAt: tsProto(row.ResolvedAt),
		CreatedAt: tsProto(row.CreatedAt),
		// Answering needs the member role, like the rpc.
		CanAnswer: row.State == store.QuestionStateOpen && row.AskedTo == c.actor() && c.p.HasRole(c.org, store.OrgRoleMember),
	}
}

// ListQuestions implements QuestionService.
func (s *Server) ListQuestions(ctx context.Context, req *questionv1alpha1.ListQuestionsRequest) (*questionv1alpha1.ListQuestionsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var rows []store.Question
	if req.GetTaskId() != "" {
		task, err := parseID("task_id", req.GetTaskId())
		if err != nil {
			return nil, err
		}
		if _, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: task}); err != nil {
			return nil, s.dbError(err, "task")
		}
		if rows, err = s.q.ListQuestionsForTask(ctx, store.ListQuestionsForTaskParams{OrgID: c.org, TaskID: task}); err != nil {
			return nil, s.dbError(err, "questions")
		}
	} else if rows, err = s.q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: c.org, UserID: c.actor()}); err != nil {
		return nil, s.dbError(err, "questions")
	}
	res := &questionv1alpha1.ListQuestionsResponse{}
	for _, r := range rows {
		// A question is the addressee's: others, admins included, do not see its text here. The
		// task timeline shows that it was asked and how it ended.
		if r.AskedTo != c.actor() || r.State == store.QuestionStateAsked {
			continue
		}
		res.Questions = append(res.Questions, questionProto(r, c))
	}
	return res, nil
}

// resolveQuestion runs one resolution in a transaction and maps its errors. The agent's sandbox is
// pre-woken after the commit, so a late answer does not wait for the reconciler to notice the run.
func (s *Server) resolveQuestion(ctx context.Context, c caller, id pgtype.UUID,
	do func(q *store.Queries) (store.Question, error),
) (store.Question, error) {
	var out store.Question
	err := s.commit(ctx, func(q *store.Queries) ([]durable.Notification, error) {
		var err error
		if out, err = do(q); err != nil {
			return nil, err
		}
		// Wake the sandbox only when this resolution made the run claimable; otherwise other
		// questions or approvals still hold it (or it is no longer parked) and a wake is noise.
		run, err := q.GetRun(ctx, store.GetRunParams{OrgID: c.org, ID: out.RunID})
		if err != nil {
			return nil, err
		}
		if run.State != store.RunStateParked || !run.ReadyAt.Valid {
			return nil, nil
		}
		task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: out.TaskID})
		if err != nil {
			return nil, err
		}
		return wakeNotification("question-resolved:"+idString(out.ID), task), nil
	})
	err = s.sendFailed(err, "waking the agent's sandbox for an answered question failed")
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, questions.ErrNotOpen):
		return out, connect.NewError(connect.CodeAborted, "the question is no longer open: it was answered, dismissed, expired or canceled")
	case errors.Is(err, questions.ErrNotAskedTo):
		return out, connect.NewError(connect.CodePermissionDenied, "this question is addressed to someone else")
	case errors.Is(err, questions.ErrInvalid):
		return out, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return out, s.dbError(err, "question")
}

// AnswerQuestion implements QuestionService.
func (s *Server) AnswerQuestion(ctx context.Context, req *questionv1alpha1.AnswerQuestionRequest) (*questionv1alpha1.AnswerQuestionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("question_id", req.GetQuestionId())
	if err != nil {
		return nil, err
	}
	var ans questions.Answer
	switch a := req.GetAnswer().(type) {
	case *questionv1alpha1.AnswerQuestionRequest_Choice:
		ans = questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: a.Choice}
	case *questionv1alpha1.AnswerQuestionRequest_Draft:
		ans = questions.Answer{Kind: store.QuestionAnswerKindDraft, Text: a.Draft}
	case *questionv1alpha1.AnswerQuestionRequest_Text:
		ans = questions.Answer{Kind: store.QuestionAnswerKindText, Text: a.Text}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "an answer is a choice, the draft or text")
	}
	row, err := s.resolveQuestion(ctx, c, id, func(q *store.Queries) (store.Question, error) {
		return questions.Respond(ctx, q, c.org, id, c.actor(), ans)
	})
	if err != nil {
		return nil, err
	}
	return &questionv1alpha1.AnswerQuestionResponse{Question: questionProto(row, c)}, nil
}

// DismissQuestion implements QuestionService.
func (s *Server) DismissQuestion(ctx context.Context, req *questionv1alpha1.DismissQuestionRequest) (*questionv1alpha1.DismissQuestionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("question_id", req.GetQuestionId())
	if err != nil {
		return nil, err
	}
	row, err := s.resolveQuestion(ctx, c, id, func(q *store.Queries) (store.Question, error) {
		return questions.Dismiss(ctx, q, c.org, id, c.actor())
	})
	if err != nil {
		return nil, err
	}
	return &questionv1alpha1.DismissQuestionResponse{Question: questionProto(row, c)}, nil
}

// NotifyQuestions sends the notification of every opened question that has none yet, through the
// server's notifier (DBOS steps with --durable, inline otherwise).
func (s *Server) NotifyQuestions(ctx context.Context) (int, error) {
	return questions.NotifyPending(ctx, s.pool, s.notifier)
}

// QuestionSweeper calls NotifyQuestions every interval until ctx ends.
func (s *Server) QuestionSweeper(ctx context.Context, interval time.Duration) {
	questions.Sweeper(ctx, s.pool, s.notifier, interval, s.log)
}
