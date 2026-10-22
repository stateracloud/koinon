// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/connectors/github/githubtest"
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

	It("has no diff for a comment", func(ctx SpecContext) {
		_, err := f.owner.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: f.org, ApprovalId: req.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("hides requests from other orgs", func(ctx SpecContext) {
		other, owner := e.newTeamOrg(ctx)
		_, err := owner.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: other, ApprovalId: req.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: other, ApprovalId: req.ID.String(), ArgsSha256: req.ArgsSha256})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: f.org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: other, ApprovalId: req.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: f.org, ApprovalId: req.ID.String()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})

var _ = Describe("ApprovalService.GetApprovalDiff", func() {
	var (
		e  *env
		f  orgFixture
		gh *githubtest.Fake
	)

	BeforeEach(func(ctx SpecContext) {
		gh = githubtest.New()
		DeferCleanup(gh.Server.Close)
		e = newEnv(ctx, envOptions{github: github.NewClient(github.Options{BaseURL: gh.URL()}), ghFake: gh})
		f = e.newOrgFixture(ctx)
		gh.SetBranch("widgets", "main", "seed", map[string]string{"docs/guide.md": "one\ntwo\n"})
	})

	// pullRequest records a create_pull_request call of the fixture's run.
	pullRequest := func(ctx context.Context, files ...githubtools.FileChange) store.ApprovalRequest {
		GinkgoHelper()
		pr := githubtools.PullRequestArgs{Branch: "koinon/docs", Title: "Docs", Files: files}
		args, err := githubtools.Canonical(pr)
		Expect(err).NotTo(HaveOccurred())
		risk, err := json.Marshal(githubtools.AssessRisk(pr))
		Expect(err).NotTo(HaveOccurred())
		row, err := store.New(e.pool).CreateApprovalRequest(ctx, store.CreateApprovalRequestParams{
			OrgID: uuid(f.org), TicketID: uuid(f.ticket.GetId()), RunID: uuid(f.run.GetId()), AgentID: uuid(f.agent.GetId()),
			Connector: "github", Tool: githubtools.ToolCreatePullRequest, Target: "github:acme/widgets",
			ArgsJson: string(args), ArgsSha256: githubtools.HashArgs(args), Risk: risk,
			Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		return row
	}

	It("shows every viewer the diff against the base branch, without deciding anything", func(ctx SpecContext) {
		row := pullRequest(ctx,
			githubtools.FileChange{Path: "docs/guide.md", Content: "one\n2\n"},
			githubtools.FileChange{Path: "docs/new.md", Content: "new\n"},
		)
		viewer := e.userIn(ctx, f.org, store.OrgRoleViewer)
		res, err := viewer.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: f.org, ApprovalId: row.ID.String()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetBase()).To(Equal("main"))
		Expect(res.GetFiles()).To(HaveLen(2))
		Expect(res.GetFiles()[0].GetPath()).To(Equal("docs/guide.md"))
		Expect(res.GetFiles()[0].GetStatus()).To(Equal(approvalv1alpha1.FileStatus_FILE_STATUS_MODIFIED))
		Expect(res.GetFiles()[0].GetUnifiedDiff()).To(Equal("--- a/docs/guide.md\n+++ b/docs/guide.md\n@@ -1,2 +1,2 @@\n one\n-two\n+2\n"))
		Expect(res.GetFiles()[1].GetStatus()).To(Equal(approvalv1alpha1.FileStatus_FILE_STATUS_ADDED))
		Expect(res.GetFiles()[1].GetUnifiedDiff()).To(Equal("--- /dev/null\n+++ b/docs/new.md\n@@ -0,0 +1 @@\n+new\n"))
		Expect(res.GetFiles()[1].GetFallbackReason()).To(BeEmpty())

		got, err := viewer.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: f.org, ApprovalId: row.ID.String()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetApproval().GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_PENDING))
		Expect(got.GetApproval().GetCanDecide()).To(BeFalse())
		Expect(gh.Snapshot("widgets").Calls).To(BeEmpty(), "nothing was written to GitHub")
	})

	It("falls back to the full new content when GitHub fails", func(ctx SpecContext) {
		gh.FailContents("widgets", "docs/guide.md", http.StatusInternalServerError)
		row := pullRequest(ctx, githubtools.FileChange{Path: "docs/guide.md", Content: "one\n2\n"})
		res, err := f.owner.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: f.org, ApprovalId: row.ID.String()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetFiles()).To(HaveLen(1))
		Expect(res.GetFiles()[0].GetUnifiedDiff()).To(BeEmpty())
		Expect(res.GetFiles()[0].GetStatus()).To(Equal(approvalv1alpha1.FileStatus_FILE_STATUS_UNSPECIFIED))
		Expect(res.GetFiles()[0].GetFallbackReason()).To(Equal("GitHub answered 500 Internal Server Error when Koinon read the current version"))
	})

	It("refuses another org's request", func(ctx SpecContext) {
		row := pullRequest(ctx, githubtools.FileChange{Path: "docs/guide.md", Content: "one\n2\n"})
		other, owner := e.newTeamOrg(ctx)
		mints := gh.Mints()
		for _, org := range []string{other, f.org} {
			res, err := owner.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: org, ApprovalId: row.ID.String()})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound), org)
			Expect(res).To(BeNil())
		}
		Expect(gh.Mints()).To(Equal(mints), "no token was minted for a refused caller")
	})
})
