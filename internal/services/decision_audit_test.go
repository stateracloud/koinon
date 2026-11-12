// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/pricing"
)

// The decision RPCs reach the approvals and questions packages, which write their audit events in
// their own transactions; every one of those events carries the request ID (issue #659), like the
// events inAuditTx writes.
var _ = Describe("decision RPCs", func() {
	var (
		pool      *pgxpool.Pool
		s         *Server
		q         *store.Queries
		org, user pgtype.UUID
		orgID     string
		run       store.Run
		ctx       context.Context
	)

	BeforeEach(func(sctx SpecContext) {
		testenv.Require(testenv.Docker)
		pool = pgtest.StartTemplate(sctx).NewDB(sctx)
		q = store.New(pool)
		org, user, _ = pgtest.NewOrg(sctx, pool)
		orgID = org.String()
		a, err := q.CreateAgent(sctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		v, err := q.CreateAgentVersion(sctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"instructions":"x"}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.ActivateAgent(sctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		t, err := q.CreateTask(sctx, store.CreateTaskParams{OrgID: org, Title: "fix", AgentID: agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateRun(sctx, store.CreateRunParams{OrgID: org, AgentID: agent.ID, TaskID: t.ID, MaxWallClockSeconds: 1800})
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(sctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		run, err = q.MarkRunRunning(sctx, store.MarkRunRunningParams{OrgID: org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())

		log := slog.New(slog.DiscardHandler)
		s = New(pool, Options{Logger: log, Prices: pricing.Embedded(), Approvals: approvals.New(pool, approvals.Options{Logger: log})})
		// A bare RPC context: a signed-in owner and no request ID of its own.
		ctx = auth.WithPrincipal(context.Background(), &auth.Principal{
			UserID: user, Memberships: []auth.Membership{{OrgID: org, Role: store.OrgRoleOwner}},
		})
	})

	// pending records a call on a connector without an executor: approving it fails the execution
	// at once (permanently), which is all this spec needs.
	pending := func(sctx SpecContext, body string) store.ApprovalRequest {
		GinkgoHelper()
		args := `{"body":"` + body + `"}`
		sum := sha256.Sum256([]byte(args))
		row, err := q.CreateApprovalRequest(sctx, store.CreateApprovalRequestParams{
			OrgID: org, TaskID: run.TaskID, RunID: run.ID, AgentID: run.AgentID,
			Connector: "nohost", Tool: "comment_on_issue", Target: "nohost:acme/widgets",
			ArgsJson: args, ArgsSha256: hex.EncodeToString(sum[:]), Risk: []byte(`{}`),
			Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		return row
	}
	question := func(sctx SpecContext, text string) store.Question {
		GinkgoHelper()
		var row store.Question
		Expect(pgx.BeginFunc(sctx, pool, func(tx pgx.Tx) error {
			qq := store.New(tx)
			var err error
			row, _, err = questions.Create(sctx, qq, questions.Ask{
				Org: org, Task: run.TaskID, Run: run.ID, Agent: run.AgentID,
				Target: questions.Target{Kind: store.NotifyTargetKindUser, User: user}, Recipients: []pgtype.UUID{user},
				Question: text, Choices: []string{"yes", "no"}, Default: "no", ExpiresAt: time.Now().Add(time.Hour),
			})
			if err != nil {
				return err
			}
			_, err = questions.OpenForRun(sctx, qq, org, run.ID, run.TaskID)
			return err
		})).To(Succeed())
		return row
	}
	requestIDs := func(sctx SpecContext, action string) []string {
		GinkgoHelper()
		rows, err := pool.Query(sctx, `SELECT request_id FROM audit_events WHERE org_id = $1 AND action = $2`, org, action)
		Expect(err).NotTo(HaveOccurred())
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		return ids
	}
	stamped := func(sctx SpecContext, action string) {
		GinkgoHelper()
		ids := requestIDs(sctx, action)
		Expect(ids).To(HaveLen(1), action)
		Expect(ids[0]).NotTo(BeEmpty(), action+" carries the request ID")
	}

	It("stamps the request ID on Approve with a scope, Deny and RevokeApprovalScope", func(sctx SpecContext) {
		approved := pending(sctx, "approve")
		res, err := s.Approve(ctx, &approvalv1alpha1.ApproveRequest{
			OrgId: orgID, ApprovalId: approved.ID.String(), ArgsSha256: approved.ArgsSha256,
			Scope: approvalv1alpha1.ScopeKind_SCOPE_KIND_AGENT, ScopeUntil: timestamppb.New(time.Now().Add(24 * time.Hour)),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetApproval().GetStatus()).NotTo(BeZero())
		var scopeID pgtype.UUID
		Expect(pool.QueryRow(sctx, `SELECT id FROM approval_scopes WHERE org_id = $1`, org).Scan(&scopeID)).To(Succeed())
		stamped(sctx, "approval.approve")
		stamped(sctx, approvals.AuditScopeCreate)

		denied := pending(sctx, "deny")
		_, err = s.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: orgID, ApprovalId: denied.ID.String(), Note: "no"})
		Expect(err).NotTo(HaveOccurred())
		stamped(sctx, "approval.deny")

		_, err = s.RevokeApprovalScope(ctx, &approvalv1alpha1.RevokeApprovalScopeRequest{OrgId: orgID, ScopeId: scopeID.String()})
		Expect(err).NotTo(HaveOccurred())
		stamped(sctx, approvals.AuditScopeRevoke)
	})

	It("stamps the request ID on AnswerQuestion and DismissQuestion", func(sctx SpecContext) {
		answered := question(sctx, "Post the reply?")
		_, err := s.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: orgID, QuestionId: answered.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "yes"},
		})
		Expect(err).NotTo(HaveOccurred())
		stamped(sctx, "question.answer")

		dismissed := question(sctx, "Close the issue?")
		_, err = s.DismissQuestion(ctx, &questionv1alpha1.DismissQuestionRequest{OrgId: orgID, QuestionId: dismissed.ID.String()})
		Expect(err).NotTo(HaveOccurred())
		stamped(sctx, "question.dismiss")
	})

	It("refuses a decision once expires_at has passed with FailedPrecondition, naming the expiry", func(sctx SpecContext) {
		row := pending(sctx, "late")
		_, err := pool.Exec(sctx, `UPDATE approval_requests SET expires_at = now() - interval '1 minute' WHERE org_id = $1 AND id = $2`, org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		for _, decide := range []func() error{
			func() error {
				_, err := s.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: orgID, ApprovalId: row.ID.String(), ArgsSha256: row.ArgsSha256})
				return err
			},
			func() error {
				_, err := s.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: orgID, ApprovalId: row.ID.String()})
				return err
			},
		} {
			err := decide()
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("expired at"))
		}
		got, err := q.GetApprovalRequest(sctx, store.GetApprovalRequestParams{OrgID: org, ID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusPending), "the sweeper marks it expired; a person cannot decide it")
		Expect(requestIDs(sctx, "approval.approve")).To(BeEmpty())
		Expect(requestIDs(sctx, "approval.deny")).To(BeEmpty())
	})
})
