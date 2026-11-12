// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1/questionv1alpha1connect"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
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

// questionNames memoises display names within one request.
type questionNames map[pgtype.UUID]string

func (s *Server) name(ctx context.Context, names questionNames, id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	if n, ok := names[id]; ok {
		return n
	}
	n, err := s.q.GetUserDisplayName(ctx, id)
	if err != nil {
		n = ""
	}
	names[id] = n
	return n
}

// questionsProto renders rows for the caller, with their recipients in one query. waiting is the
// time each question started waiting on the caller (the inbox rows carry it; otherwise it is read
// from the recipients).
func (s *Server) questionsProto(ctx context.Context, c caller, rows []store.Question, waiting map[pgtype.UUID]pgtype.Timestamptz) ([]*questionv1alpha1.Question, error) {
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	recips, err := s.q.ListQuestionRecipients(ctx, store.ListQuestionRecipientsParams{OrgID: c.org, QuestionIds: ids})
	if err != nil {
		return nil, err
	}
	byQuestion := map[pgtype.UUID][]store.ListQuestionRecipientsRow{}
	for _, r := range recips {
		byQuestion[r.QuestionID] = append(byQuestion[r.QuestionID], r)
	}
	names := questionNames{}
	out := make([]*questionv1alpha1.Question, 0, len(rows))
	for _, row := range rows {
		rs := byQuestion[row.ID]
		isRecipient := false
		var viewerWaiting pgtype.Timestamptz
		for _, r := range rs {
			if r.UserID == c.actor() {
				isRecipient, viewerWaiting = true, r.WaitingSince
			}
		}
		if w, ok := waiting[row.ID]; ok {
			viewerWaiting = w
		}
		target := questions.TargetOf(row)
		label := questions.Label(target, s.name(ctx, names, target.User))
		p := &questionv1alpha1.Question{
			Id: idString(row.ID), OrgId: idString(row.OrgID), TaskId: idString(row.TaskID), RunId: idString(row.RunID),
			AgentId: idString(row.AgentID), Question: row.Question,
			Choices: questions.ChoicesOf(row), Draft: row.Draft.String, DefaultAnswer: row.DefaultAnswer,
			State: questionStateToProto[row.State], Answer: row.Answer.String,
			AnswerKind: answerKindToProto[row.AnswerKind.QuestionAnswerKind], AnsweredBy: idString(row.AnsweredBy),
			AnsweredByName: s.name(ctx, names, row.AnsweredBy),
			ExpiresAt:      tsProto(row.ExpiresAt), OpenedAt: tsProto(row.OpenedAt), ResolvedAt: tsProto(row.ResolvedAt),
			CreatedAt: tsProto(row.CreatedAt),
			Target:    questionTargetProto(target), TargetLabel: label,
			// Answering needs the member role, like the rpc, and a place among the recipients.
			CanAnswer: row.State == store.QuestionStateOpen && isRecipient && c.p.HasRole(c.org, store.OrgRoleMember),
		}
		if isRecipient {
			p.WaitingSince = tsProto(viewerWaiting)
		}
		for _, r := range rs {
			p.Recipients = append(p.Recipients, &questionv1alpha1.Recipient{
				UserId: idString(r.UserID), DisplayName: r.DisplayName, WaitingSince: tsProto(r.WaitingSince),
			})
		}
		// Deprecated asked_to: the first recipient by user ID, for older clients.
		first := ""
		for _, r := range p.Recipients {
			if first == "" || r.GetUserId() < first {
				first = r.GetUserId()
			}
		}
		p.AskedTo = first //nolint:staticcheck // deprecated field, still set for older clients
		out = append(out, p)
	}
	return out, nil
}

// questionTargetProto is the wire form of a question's target.
func questionTargetProto(t questions.Target) *templatev1alpha1.NotifyTarget {
	p := &templatev1alpha1.NotifyTarget{ChannelId: t.Channel}
	switch t.Kind {
	case store.NotifyTargetKindUser:
		p.Kind, p.UserId = templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER, idString(t.User)
	case store.NotifyTargetKindRole:
		p.Kind, p.Role = templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_ROLE, auth.RoleToProto(t.Role)
	case store.NotifyTargetKindChannel:
		p.Kind = templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_CHANNEL
	}
	return p
}

// ListQuestions implements QuestionService.
func (s *Server) ListQuestions(ctx context.Context, req *questionv1alpha1.ListQuestionsRequest) (*questionv1alpha1.ListQuestionsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var rows []store.Question
	waiting := map[pgtype.UUID]pgtype.Timestamptz{}
	if req.GetTaskId() != "" {
		task, err := parseID("task_id", req.GetTaskId())
		if err != nil {
			return nil, err
		}
		if _, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: task}); err != nil {
			return nil, s.dbError(err, "task")
		}
		all, err := s.q.ListQuestionsForTask(ctx, store.ListQuestionsForTaskParams{OrgID: c.org, TaskID: task})
		if err != nil {
			return nil, s.dbError(err, "questions")
		}
		// A question is the recipients': others, admins included, do not see its text here. The
		// task timeline shows that it was asked and how it ended.
		ids := make([]pgtype.UUID, 0, len(all))
		for _, r := range all {
			ids = append(ids, r.ID)
		}
		recips, err := s.q.ListQuestionRecipients(ctx, store.ListQuestionRecipientsParams{OrgID: c.org, QuestionIds: ids})
		if err != nil {
			return nil, s.dbError(err, "questions")
		}
		mine := map[pgtype.UUID]bool{}
		for _, r := range recips {
			if r.UserID == c.actor() {
				mine[r.QuestionID] = true
			}
		}
		for _, r := range all {
			if mine[r.ID] && r.State != store.QuestionStateAsked {
				rows = append(rows, r)
			}
		}
	} else {
		inbox, err := s.q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: c.org, UserID: c.actor()})
		if err != nil {
			return nil, s.dbError(err, "questions")
		}
		for _, r := range inbox {
			rows = append(rows, r.Question)
			waiting[r.Question.ID] = r.WaitingSince
		}
	}
	res := &questionv1alpha1.ListQuestionsResponse{}
	if res.Questions, err = s.questionsProto(ctx, c, rows, waiting); err != nil {
		return nil, s.dbError(err, "questions")
	}
	return res, nil
}

// ListAskableMembers implements QuestionService.
func (s *Server) ListAskableMembers(ctx context.Context, req *questionv1alpha1.ListAskableMembersRequest) (*questionv1alpha1.ListAskableMembersResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListAskableMembers(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "members")
	}
	res := &questionv1alpha1.ListAskableMembersResponse{}
	for _, r := range rows {
		res.Members = append(res.Members, &questionv1alpha1.AskableMember{
			UserId: idString(r.ID), Email: r.Email.String, DisplayName: r.DisplayName, Role: auth.RoleToProto(r.Role),
		})
	}
	return res, nil
}

// resolveQuestion runs one resolution in a transaction and maps its errors. The agent's sandbox is
// pre-woken after the commit, so a late answer does not wait for the reconciler to notice the run.
func (s *Server) resolveQuestion(ctx context.Context, c caller, id pgtype.UUID,
	do func(q *store.Queries, b audit.Batch) (store.Question, error),
) (store.Question, error) {
	var out store.Question
	err := s.commit(ctx, func(q *store.Queries, b audit.Batch) ([]durable.Notification, error) {
		var err error
		if out, err = do(q, b); err != nil {
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
		if out.State == store.QuestionStateAnswered && out.AnsweredBy.Valid && out.AnsweredBy != c.actor() {
			if name := s.name(ctx, questionNames{}, out.AnsweredBy); name != "" {
				return out, connect.NewError(connect.CodeAborted, name+" already answered this question")
			}
		}
		return out, connect.NewError(connect.CodeAborted, "the question is no longer open: it was answered, dismissed, expired or canceled")
	case errors.Is(err, questions.ErrNotAskedTo):
		return out, connect.NewError(connect.CodePermissionDenied, "this question is not addressed to you")
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
	ctx = requestCtx(ctx) // the answer's audit event carries the request ID
	row, err := s.resolveQuestion(ctx, c, id, func(q *store.Queries, b audit.Batch) (store.Question, error) {
		return questions.Respond(ctx, q, b, c.org, id, c.actor(), ans)
	})
	if err != nil {
		return nil, err
	}
	ps, err := s.questionsProto(ctx, c, []store.Question{row}, nil)
	if err != nil {
		return nil, s.dbError(err, "question")
	}
	return &questionv1alpha1.AnswerQuestionResponse{Question: ps[0]}, nil
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
	ctx = requestCtx(ctx) // the dismissal's audit event carries the request ID
	row, err := s.resolveQuestion(ctx, c, id, func(q *store.Queries, b audit.Batch) (store.Question, error) {
		return questions.Dismiss(ctx, q, b, c.org, id, c.actor())
	})
	if err != nil {
		return nil, err
	}
	ps, err := s.questionsProto(ctx, c, []store.Question{row}, nil)
	if err != nil {
		return nil, s.dbError(err, "question")
	}
	return &questionv1alpha1.DismissQuestionResponse{Question: ps[0]}, nil
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
