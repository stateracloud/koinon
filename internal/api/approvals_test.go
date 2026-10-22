// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/db/store"
	approvalv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/procoduck/koinon/internal/githubtools"
)

// pendingApproval records a comment_on_issue call of the fixture's run, as the tool server
// would.
func (e *env) pendingApproval(ctx context.Context, f orgFixture) store.ApprovalRequest {
	GinkgoHelper()
	args, err := githubtools.Canonical(githubtools.CommentArgs{IssueNumber: 7, Body: "Opened a PR."})
	Expect(err).NotTo(HaveOccurred())
	req, err := store.New(e.pool).CreateApprovalRequest(ctx, store.CreateApprovalRequestParams{
		OrgID: uuid(f.org), TicketID: uuid(f.ticket.GetId()), RunID: uuid(f.run.GetId()), AgentID: uuid(f.agent.GetId()),
		Connector: "github", Tool: githubtools.ToolCommentOnIssue, Target: "github:acme/widgets",
		ArgsJson: string(args), ArgsSha256: githubtools.HashArgs(args), Risk: []byte(`{"files":0,"bytes":0}`),
		Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	Expect(err).NotTo(HaveOccurred())
	return req
}

// approveStatus calls Approve as plain Connect JSON over HTTP and returns the HTTP status, to
// check what a browser sees.
func approveStatus(ctx context.Context, c *client, org, id, sha string) int {
	GinkgoHelper()
	body := fmt.Sprintf(`{"orgId":%q,"approvalId":%q,"argsSha256":%q}`, org, id, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.b.e.srv.URL+"/koinon.approval.v1alpha1.ApprovalService/Approve", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.b.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

var _ = Describe("ApprovalService", func() {
	var (
		e   *env
		f   orgFixture
		req store.ApprovalRequest
	)

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		f = e.newOrgFixture(ctx)
		req = e.pendingApproval(ctx, f)
	})

	It("shows pending requests to viewers read-only and lets only admins and owners decide", func(ctx SpecContext) {
		member := e.userIn(ctx, f.org, store.OrgRoleMember)
		admin := e.userIn(ctx, f.org, store.OrgRoleAdmin)
		id := req.ID.String()

		list, err := member.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: f.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		Expect(list.GetApprovals()[0].GetCanDecide()).To(BeFalse())
		Expect(list.GetApprovals()[0].GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_PENDING))

		got, err := member.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: f.org, ApprovalId: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetApproval().GetComment().GetBody()).To(Equal("Opened a PR."))
		Expect(got.GetApproval().GetArgsSha256()).To(Equal(req.ArgsSha256))

		_, err = member.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: f.org, ApprovalId: id, ArgsSha256: req.ArgsSha256})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.approvals.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: f.org, ApprovalId: id})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("an admin with the wrong hash is refused with Aborted (HTTP 409) and nothing changes")
		_, err = admin.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: f.org, ApprovalId: id, ArgsSha256: githubtools.HashArgs([]byte("{}"))})
		Expect(codeOf(err)).To(Equal(connect.CodeAborted))
		Expect(approveStatus(ctx, admin, f.org, id, githubtools.HashArgs([]byte("{}")))).To(Equal(http.StatusConflict))
		got, err = admin.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: f.org, ApprovalId: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetApproval().GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_PENDING))
		Expect(got.GetApproval().GetCanDecide()).To(BeTrue())

		By("an admin denies it")
		denied, err := admin.approvals.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: f.org, ApprovalId: id, Note: "not now"})
		Expect(err).NotTo(HaveOccurred())
		Expect(denied.GetApproval().GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_DENIED))
		Expect(denied.GetApproval().GetDecisionNote()).To(Equal("not now"))
		list, err = member.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: f.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(BeEmpty())
		list, err = member.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: f.org, TicketId: f.ticket.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		By("approving a request that is no longer pending stays FailedPrecondition (HTTP 400)")
		_, err = f.owner.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: f.org, ApprovalId: id, ArgsSha256: req.ArgsSha256})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(approveStatus(ctx, f.owner, f.org, id, req.ArgsSha256)).To(Equal(http.StatusBadRequest))
	})

	It("hides requests from other orgs", func(ctx SpecContext) {
		other, owner := e.newTeamOrg(ctx)
		_, err := owner.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: other, ApprovalId: req.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: other, ApprovalId: req.ID.String(), ArgsSha256: req.ArgsSha256})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: f.org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
