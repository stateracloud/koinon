// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/approvals"
	"github.com/procoduck/koinon/internal/connectors/github/githubtest"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/githubtools"
)

// prArgs proposes text, a byte-exact edge case (no trailing newline, CRLF, tab, non-ASCII,
// a script) and the deletion of README.md.
func prArgs() map[string]any {
	return map[string]any{
		"branch": "koinon/fix-typo", "title": "Fix the typo", "body": "Fixes the typo in the docs.",
		"files": []map[string]any{
			{"path": "docs/guide.md", "content": "# Guide\r\n\tcafé ☕ — no newline at the end"},
			{"path": "scripts/check.sh", "content": "#!/bin/sh\nexit 0\n"},
			{"path": "README.md", "delete": true},
		},
	}
}

func result(row store.ApprovalRequest) map[string]any {
	var m map[string]any
	ExpectWithOffset(1, json.Unmarshal(row.Result, &m)).To(Succeed())
	return m
}

var _ = Describe("Service", func() {
	It("approves the exact call and creates exactly one branch, commit and pull request", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		Expect(req.Status).To(Equal("pending"))

		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "looks good")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		Expect(row.Executions).To(BeEquivalentTo(1))
		Expect(row.DecidedBy).To(Equal(f.admin))
		Expect(row.DecisionNote.String).To(Equal("looks good"))
		res := result(row)
		Expect(res).To(HaveKeyWithValue("url", "https://github.test/acme/widgets/pull/1"))
		Expect(res).To(HaveKeyWithValue("branch", "koinon/fix-typo"))
		Expect(res).To(HaveKeyWithValue("base", "main"))

		By("one branch, one commit on top of main, one pull request")
		repo := f.gh.Snapshot("widgets")
		Expect(repo.Refs).To(HaveLen(2))
		Expect(repo.Calls).To(Equal(map[string]int{
			"POST git/blobs": 2, "POST git/trees": 1, "POST git/commits": 1, "POST git/refs": 1, "POST pulls": 1,
		}))
		Expect(repo.Pulls).To(HaveLen(1))
		pr := repo.Pulls[0]
		Expect(pr.Head).To(Equal("koinon/fix-typo"))
		Expect(pr.Base).To(Equal("main"))
		Expect(pr.Title).To(Equal("Fix the typo"))
		Expect(pr.Body).To(Equal("Fixes the typo in the docs."))
		head := repo.Commits[repo.Refs["koinon/fix-typo"]]
		Expect(head.Parents).To(Equal([]string{repo.Refs["main"]}))
		Expect(pr.HeadSHA).To(Equal(head.SHA))

		By("the commit holds exactly the approved files, byte for byte")
		Expect(f.gh.Files("widgets", "koinon/fix-typo")).To(Equal(map[string][]byte{
			"docs/guide.md":    []byte("# Guide\r\n\tcafé ☕ — no newline at the end"),
			"scripts/check.sh": []byte("#!/bin/sh\nexit 0\n"),
		}))

		By("the trailers name the ticket's creator, the approver and the request")
		Expect(head.Message).To(Equal("Fix the typo\n\nFixes the typo in the docs.\n\n" +
			"Requested-by: Olive <olive@example.test>\nApproved-by: Adam <adam@example.test>\n" +
			"Koinon-Approval: " + req.ID + "\n"))

		By("the token was narrowed to the one repository and the permissions the tool needs")
		Expect(f.gh.Requests()).To(Equal([]githubtest.Request{{
			Repositories: []string{"widgets"},
			Permissions:  map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"},
		}}))
		Expect(f.logs.String()).NotTo(ContainSubstring("ghs_"), "tokens are never logged")
		var stored string
		Expect(pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM approval_requests a WHERE id = $1`, row.ID).Scan(&stored)).To(Succeed())
		Expect(stored).NotTo(ContainSubstring("ghs_"), "tokens are never stored")

		By("the ticket timeline records the request, the decision and the outcome")
		evs := f.ticketEvents(ctx)
		Expect(kinds(evs)).To(Equal([]string{approvals.KindRequested, approvals.KindApproved, approvals.KindExecuted}))
		Expect(evs[1].ActorUserID).To(Equal(f.admin))
		Expect(string(evs[2].Payload)).To(ContainSubstring("https://github.test/acme/widgets/pull/1"))
	})

	It("returns the same pull request on a replayed approval and after a crashed execution", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		first, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())

		By("a second approval with the same hash, by another approver")
		again, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.owner, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Status).To(Equal(store.ApprovalStatusExecuted))
		Expect(again.Executions).To(BeEquivalentTo(1))
		Expect(result(again)["url"]).To(Equal(result(first)["url"]))
		Expect(again.DecidedBy).To(Equal(f.admin), "the first decision stands")

		By("an executor that died after the GitHub calls, before recording the outcome")
		_, err = pool.Exec(ctx, `UPDATE approval_requests SET status = 'approved', result = NULL, executed_at = NULL WHERE id = $1`, first.ID)
		Expect(err).NotTo(HaveOccurred())
		replayed, err := f.svc.Execute(ctx, f.org, first.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(replayed.Status).To(Equal(store.ApprovalStatusExecuted))
		Expect(replayed.Executions).To(BeEquivalentTo(2))
		Expect(result(replayed)["url"]).To(Equal(result(first)["url"]))
		Expect(result(replayed)["commit"]).To(Equal(result(first)["commit"]))

		repo := f.gh.Snapshot("widgets")
		Expect(repo.Pulls).To(HaveLen(1))
		Expect(repo.Calls["POST pulls"]).To(Equal(1))
		Expect(repo.Calls["POST git/refs"]).To(Equal(1))
		Expect(repo.Calls["POST git/commits"]).To(Equal(1))
	})

	It("refuses an approval with another hash and executes nothing", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		changed := prArgs()
		changed["base"] = "release"
		other, err := githubtools.Canonical(changed)
		Expect(err).NotTo(HaveOccurred())

		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, githubtools.HashArgs(other), "")
		Expect(err).To(MatchError(approvals.ErrHashMismatch))
		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, strings.Repeat("0", 64), "")
		Expect(err).To(MatchError(approvals.ErrHashMismatch))

		row := f.get(ctx, req.ID)
		Expect(row.Status).To(Equal(store.ApprovalStatusPending))
		Expect(row.Executions).To(BeZero())
		Expect(row.DecidedBy.Valid).To(BeFalse())
		Expect(f.gh.Mints()).To(BeZero(), "no token was even minted")
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
		Expect(kinds(f.ticketEvents(ctx))).To(Equal([]string{approvals.KindRequested}))
	})

	It("denies with a note and resumes the parked run", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)
		Expect(f.getTicket(ctx).Status).To(Equal(store.TicketStatusWaitingOnYou))

		row, err := f.svc.Deny(ctx, f.org, mustID(req.ID), f.owner, "use a smaller change")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusDenied))
		Expect(row.DecisionNote.String).To(Equal("use a smaller change"))

		run := f.getRun(ctx)
		Expect(run.State).To(Equal(store.RunStateParked))
		Expect(run.ReadyAt.Valid).To(BeTrue(), "the run is claimable again")
		Expect(f.getTicket(ctx).Status).To(Equal(store.TicketStatusInProgress))
		Expect(kinds(f.ticketEvents(ctx))).To(Equal([]string{approvals.KindRequested, approvals.KindDenied}))

		By("a denied request cannot be approved afterwards")
		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.owner, req.ArgsSHA256, "")
		Expect(err).To(MatchError(approvals.ErrNotPending))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
	})

	It("keeps the run parked until every request of the run is resolved", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		pr := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		comment := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 7, "body": "Opened a PR."})
		f.park(ctx)

		_, err := f.svc.Deny(ctx, f.org, mustID(comment.ID), f.owner, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.getRun(ctx).ReadyAt.Valid).To(BeFalse())
		Expect(f.getTicket(ctx).Status).To(Equal(store.TicketStatusWaitingOnYou))

		_, err = f.svc.Approve(ctx, f.org, mustID(pr.ID), f.owner, pr.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.getRun(ctx).ReadyAt.Valid).To(BeTrue())
		Expect(f.getTicket(ctx).Status).To(Equal(store.TicketStatusInProgress))
	})

	It("executes a call the Trusted preset allows without a card", func(ctx SpecContext) {
		f := newFixture(ctx, "trusted")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		Expect(req.Status).To(Equal("approved"))

		Eventually(func() store.ApprovalStatus { return f.get(ctx, req.ID).Status }).
			WithTimeout(10 * time.Second).Should(Equal(store.ApprovalStatusExecuted))
		row := f.get(ctx, req.ID)
		Expect(row.Decision).To(Equal(store.ApprovalDecisionAllow))
		Expect(row.DecidedBy.Valid).To(BeFalse())
		pending, err := q.ListPendingApprovalRequests(ctx, f.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty(), "no card")
		head := f.gh.Snapshot("widgets")
		Expect(head.Pulls).To(HaveLen(1))
		Expect(head.Commits[head.Refs["koinon/fix-typo"]].Message).To(ContainSubstring("Approved-by: Koinon Trusted preset"))

		By("the sweeper executes an allowed call the tool server could not")
		f2 := newFixture(ctx, "trusted")
		_, err = pool.Exec(ctx, `UPDATE approval_requests SET execution_lease_until = now() + interval '1 hour' WHERE org_id = $1`, f2.org)
		Expect(err).NotTo(HaveOccurred())
		args := prArgs()
		args["branch"] = "koinon/other"
		req2 := f2.call(ctx, githubtools.ToolCreatePullRequest, args)
		Eventually(func() store.ApprovalStatus { return f2.get(ctx, req2.ID).Status }).
			WithTimeout(10 * time.Second).Should(Equal(store.ApprovalStatusExecuted))
	})

	It("posts an approved comment once, with the request as idempotency key", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 7, "body": "Opened a PR."})
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		Expect(f.gh.Requests()[0].Permissions).To(Equal(map[string]string{"metadata": "read", "issues": "write"}))

		_, err = pool.Exec(ctx, `UPDATE approval_requests SET status = 'approved' WHERE id = $1`, row.ID)
		Expect(err).NotTo(HaveOccurred())
		again, err := f.svc.Execute(ctx, f.org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(result(again)["url"]).To(Equal(result(row)["url"]))
		comments := f.gh.Snapshot("widgets").Comments[7]
		Expect(comments).To(HaveLen(1))
		Expect(comments[0].Body).To(Equal("Opened a PR.\n\n" + approvals.CommentMarker(req.ID)))
	})

	It("fails, without touching it, when the branch belongs to someone else", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		f.gh.SetBranch("widgets", "koinon/fix-typo", "a human's commit", map[string]string{"x.txt": "x"})
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("was not created for this approval"))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
		Expect(kinds(f.ticketEvents(ctx))).To(ContainElement(approvals.KindFailed))
	})

	It("expires a request nobody decided and cancels its parked run with a ticket event", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)

		By("before the TTL nothing happens")
		_, err := f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusPending))

		By("after 7 days")
		_, err = pool.Exec(ctx, `UPDATE approval_requests SET expires_at = now() - interval '1 minute' WHERE id = $1`, mustID(req.ID))
		Expect(err).NotTo(HaveOccurred())
		res, err := f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Expired).To(BeNumerically(">=", 1))
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusExpired))
		run := f.getRun(ctx)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(ContainSubstring("expired"))
		Expect(f.getTicket(ctx).Status).To(Equal(store.TicketStatusTodo))
		evs := f.ticketEvents(ctx)
		Expect(kinds(evs)).To(Equal([]string{approvals.KindRequested, approvals.KindExpired, "run.canceled"}))
		Expect(string(evs[2].Payload)).To(ContainSubstring(`approval_expired`))

		By("an expired request cannot be approved")
		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).To(MatchError(approvals.ErrNotPending))
		Expect(f.gh.Mints()).To(BeZero())
	})

	It("cancels pending requests of a run that was canceled", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 7, "body": "x"})
		f.park(ctx)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: f.org, ID: f.run.ID, OutcomeReason: "user"})
		Expect(err).NotTo(HaveOccurred())

		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).To(MatchError(approvals.ErrNotPending))
		_, err = f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusCanceled))
		Expect(f.gh.Mints()).To(BeZero())
	})
})
