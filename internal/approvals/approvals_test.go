// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/runpark"
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
		Expect(res).To(HaveKeyWithValue("branch", f.branch("fix-typo")))
		Expect(res).To(HaveKeyWithValue("base", "main"))

		By("one branch, one commit on top of main, one pull request")
		repo := f.gh.Snapshot("widgets")
		Expect(repo.Refs).To(HaveLen(2))
		Expect(repo.Calls).To(Equal(map[string]int{
			"POST git/blobs": 2, "POST git/trees": 1, "POST git/commits": 1, "POST git/refs": 1, "POST pulls": 1,
		}))
		Expect(repo.Pulls).To(HaveLen(1))
		pr := repo.Pulls[0]
		Expect(pr.Head).To(Equal(f.branch("fix-typo")))
		Expect(pr.Base).To(Equal("main"))
		Expect(pr.Title).To(Equal("Fix the typo"))
		Expect(pr.Body).To(Equal("Fixes the typo in the docs."))
		head := repo.Commits[repo.Refs[f.branch("fix-typo")]]
		Expect(head.Parents).To(Equal([]string{repo.Refs["main"]}))
		Expect(pr.HeadSHA).To(Equal(head.SHA))

		By("the commit holds exactly the approved files, byte for byte")
		Expect(f.gh.Files("widgets", f.branch("fix-typo"))).To(Equal(map[string][]byte{
			"docs/guide.md":    []byte("# Guide\r\n\tcafé ☕ — no newline at the end"),
			"scripts/check.sh": []byte("#!/bin/sh\nexit 0\n"),
		}))

		By("the trailers name the task's creator, the approver and the request")
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

		By("the task timeline records the request, the decision and the outcome")
		evs := f.taskEvents(ctx)
		Expect(kinds(evs)).To(Equal([]string{approvals.KindRequested, approvals.KindApproved, approvals.KindExecuted}))
		Expect(evs[1].ActorUserID).To(Equal(f.admin))
		Expect(string(evs[2].Payload)).To(ContainSubstring("https://github.test/acme/widgets/pull/1"))

		By("the audit log records who approved, once")
		Expect(f.decisions(ctx)).To(Equal([]string{"approval.approve user " + f.admin.String()}))
		var summary string
		Expect(pool.QueryRow(ctx, `SELECT summary::text FROM audit_events WHERE org_id = $1 AND action = 'approval.approve'`, f.org).Scan(&summary)).To(Succeed())
		Expect(summary).To(ContainSubstring(req.ArgsSHA256))
		Expect(summary).To(ContainSubstring("looks good"))
		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "looks good")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.decisions(ctx)).To(HaveLen(1), "a replay is not a new decision")
	})

	It("opens the pull request on a GitHub Enterprise Server: the App flow and the Git Data API go to the connection's host, trusting its CA bundle", func(ctx SpecContext) {
		f := newGHESFixture(ctx, "standard")
		Expect(f.gh.URL()).To(HaveSuffix("/api/v3"))
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		Expect(req.Status).To(Equal("pending"))
		f.park(ctx)

		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "looks good")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		Expect(result(row)).To(HaveKeyWithValue("branch", f.branch("fix-typo")))

		By("the token was minted at the GHES, and the branch, commit and pull request are there")
		Expect(f.gh.Mints()).To(Equal(1))
		repo := f.gh.Snapshot("widgets")
		Expect(repo.Calls).To(Equal(map[string]int{
			"POST git/blobs": 2, "POST git/trees": 1, "POST git/commits": 1, "POST git/refs": 1, "POST pulls": 1,
		}))
		Expect(repo.Pulls).To(HaveLen(1))
		Expect(f.gh.Files("widgets", f.branch("fix-typo"))).To(HaveKey("docs/guide.md"))
		Expect(f.logs.String()).NotTo(ContainSubstring("ghs_"))
	})

	It("fails a GitHub Enterprise Server call when its CA bundle is missing, without falling back to github.com", func(ctx SpecContext) {
		f := newGHESFixture(ctx, "standard")
		_, err := pool.Exec(ctx, `UPDATE connections SET config = config - 'caBundle' WHERE org_id = $1`, f.org)
		Expect(err).NotTo(HaveOccurred())
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).NotTo(Equal(store.ApprovalStatusExecuted))
		// A TLS failure is a network error: the request stays approved for the sweeper's
		// retry, so the reason is in the log, not in the row.
		Expect(row.Status).To(Equal(store.ApprovalStatusApproved))
		Expect(f.logs.String()).To(ContainSubstring("certificate"))
		Expect(f.gh.Mints()).To(BeZero())
		Expect(f.gh.Snapshot("widgets").Pulls).To(BeEmpty())
	})

	It("writes files marked executable as mode 100755 and every other file as 100644", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, map[string]any{
			"branch": "koinon/add-check", "title": "Add a check script",
			"files": []map[string]any{
				{"path": "scripts/check.sh", "content": "#!/bin/sh\nexit 0\n", "executable": true},
				{"path": "scripts/lib.sh", "content": "# sourced, not run\n"},
				{"path": "docs/check.md", "content": "Run ./scripts/check.sh\n", "executable": false},
			},
		})
		Expect(req.Status).To(Equal("pending"))
		Expect(f.get(ctx, req.ID).ArgsJson).To(ContainSubstring(`"executable":true`), "the hashed arguments carry the flag")

		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		Expect(f.gh.FileModes("widgets", f.branch("add-check"))).To(Equal(map[string]string{
			"README.md":        "100644",
			"scripts/check.sh": "100755",
			"scripts/lib.sh":   "100644",
			"docs/check.md":    "100644",
		}))
	})

	DescribeTable("keeps the base mode of an unmarked modified file",
		func(ctx SpecContext, truncate bool) {
			f := newFixture(ctx, "standard")
			f.gh.SetBranchModes("widgets", "main", "add scripts", map[string]string{
				"scripts/run.sh": "#!/bin/sh\necho old\n", "scripts/lib.sh": "# old\n", "tools/deep/x/build": "#!/bin/sh\n",
			}, map[string]string{"scripts/run.sh": "100755", "tools/deep/x/build": "100755"})
			f.gh.TruncateTrees = truncate
			req := f.call(ctx, githubtools.ToolCreatePullRequest, map[string]any{
				"branch": "koinon/edit-scripts", "title": "Edit the scripts",
				"files": []map[string]any{
					{"path": "scripts/run.sh", "content": "#!/bin/sh\necho new\n"},
					{"path": "tools/deep/x/build", "content": "#!/bin/sh\necho build\n"},
					{"path": "scripts/lib.sh", "content": "# now run\n", "executable": true},
					{"path": "scripts/new.sh", "content": "#!/bin/sh\n"},
					{"path": "README.md", "content": "# widgets\n"},
				},
			})
			row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
			Expect(f.gh.FileModes("widgets", f.branch("edit-scripts"))).To(Equal(map[string]string{
				"README.md":          "100644",
				"scripts/run.sh":     "100755", // was executable, not marked: stays executable
				"tools/deep/x/build": "100755",
				"scripts/lib.sh":     "100755", // marked: becomes executable
				"scripts/new.sh":     "100644", // new and not marked: regular
			}))
		},
		Entry("from one recursive tree listing", false),
		Entry("directory by directory when GitHub truncates the listing", true),
	)

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
		Expect(kinds(f.taskEvents(ctx))).To(Equal([]string{approvals.KindRequested}))
	})

	It("refuses a directory configured on the connection when the call is made and again when it runs", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		pr := func(path string) map[string]any {
			return map[string]any{"branch": "koinon/ci", "title": "CI", "files": []map[string]any{{"path": path, "content": "x"}}}
		}
		By("recorded while the directory is not configured")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, pr("ci/pipelines/build.yml"))

		By("an admin configures the directory on the connection")
		_, err := pool.Exec(ctx, `UPDATE connections SET config = config || '{"ciDirs":["ci/pipelines"]}'::jsonb WHERE org_id = $1`, f.org)
		Expect(err).NotTo(HaveOccurred())

		By("the new call is refused at the tool server")
		Expect(f.callRefused(ctx, githubtools.ToolCreatePullRequest, pr("ci/pipelines/other.yml"))).To(ContainSubstring("under ci/pipelines"))
		Expect(f.callRefused(ctx, githubtools.ToolCreatePullRequest, pr(".github/workflows/ci.yml"))).To(ContainSubstring("under .github/workflows/"))

		By("the recorded call is refused when it is approved, and nothing is written")
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("under ci/pipelines"))
		Expect(f.gh.Snapshot("widgets").Calls).NotTo(HaveKey("POST git/commits"))
	})

	It("fails a recorded call of an unknown tool permanently and mints no token", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 7, "body": "x"})
		_, err := pool.Exec(ctx, `UPDATE approval_requests SET tool = 'delete_repository' WHERE id = $1`, mustID(req.ID))
		Expect(err).NotTo(HaveOccurred())

		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring(`unknown GitHub write tool "delete_repository"`))
		Expect(f.gh.Mints()).To(BeZero(), "nothing is executed, not even as a comment")
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
	})

	It("still denies while the install is held: Deny starts nothing outward (ADR 0107)", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)
		held := approvals.New(pool, approvals.Options{RestoreHold: restorehold.NewGuard(true)})

		row, err := held.Deny(ctx, f.org, mustID(req.ID), f.owner, "held")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusDenied))
		_, err = held.Approve(ctx, f.org, mustID(req.ID), f.owner, req.ArgsSHA256, "")
		Expect(err).To(MatchError(restorehold.ErrHeld))
	})

	It("denies with a note and resumes the parked run", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateInputRequired))

		row, err := f.svc.Deny(ctx, f.org, mustID(req.ID), f.owner, "use a smaller change")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusDenied))
		Expect(row.DecisionNote.String).To(Equal("use a smaller change"))

		run := f.getRun(ctx)
		Expect(run.State).To(Equal(store.RunStateParked))
		Expect(run.ReadyAt.Valid).To(BeTrue(), "the run is claimable again")
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateWorking))
		Expect(kinds(f.taskEvents(ctx))).To(Equal([]string{approvals.KindRequested, approvals.KindDenied}))
		Expect(f.decisions(ctx)).To(Equal([]string{"approval.deny user " + f.owner.String()}))

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
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateInputRequired))

		_, err = f.svc.Approve(ctx, f.org, mustID(pr.ID), f.owner, pr.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.getRun(ctx).ReadyAt.Valid).To(BeTrue())
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateWorking))
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
		Expect(head.Commits[head.Refs[f.branch("fix-typo")]].Message).To(ContainSubstring("Approved-by: Koinon Trusted preset"))

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

	It("fails, without touching it, when the branch appears after the request, owned by someone else", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.gh.SetBranch("widgets", f.branch("fix-typo"), "a human's commit", map[string]string{"x.txt": "x"})
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(row.Error.String).To(ContainSubstring("was not created for this approval"))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty())
		Expect(kinds(f.taskEvents(ctx))).To(ContainElement(approvals.KindFailed))
	})

	Describe("the branch namespace (old issue 268)", func() {
		It("refuses, before any approval exists, a branch that is already on the code host", func(ctx SpecContext) {
			f := newFixture(ctx, "standard")
			f.gh.SetBranch("widgets", f.branch("fix-typo"), "a human's commit", map[string]string{"x.txt": "x"})
			reason := f.callRefused(ctx, githubtools.ToolCreatePullRequest, prArgs())
			Expect(reason).To(ContainSubstring(f.branch("fix-typo")))
			Expect(reason).To(ContainSubstring("already exists"))
			Expect(reason).To(ContainSubstring("choose another branch name"))
			rows, err := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: f.org, TaskID: f.task.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty(), "nothing for a human to approve")
		})

		It("answers the same call again with its request once the approval created the branch", func(ctx SpecContext) {
			f := newFixture(ctx, "standard")
			req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
			row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
			Expect(f.gh.Snapshot("widgets").Refs).To(HaveKey(f.branch("fix-typo")))

			again := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
			Expect(again.ID).To(Equal(req.ID), "the recorded call, not a refusal of its own branch")
			Expect(again.Status).To(Equal(string(store.ApprovalStatusExecuted)))
		})

		It("records and shows the final branch name, whether or not the agent added koinon/", func(ctx SpecContext) {
			f := newFixture(ctx, "standard")
			for _, suggestion := range []string{"fix-typo", "koinon/fix-typo"} {
				args := prArgs()
				args["branch"] = suggestion
				req := f.call(ctx, githubtools.ToolCreatePullRequest, args)
				row := f.get(ctx, req.ID)
				call, err := githubtools.DecodeCall(row.Tool, []byte(row.ArgsJson))
				Expect(err).NotTo(HaveOccurred())
				Expect(call.(*githubtools.PullRequestArgs).Branch).To(Equal(f.branch("fix-typo")))
			}
		})

		It("creates both pull requests when two agents suggest the same branch", func(ctx SpecContext) {
			a := newFixture(ctx, "standard")
			b := newFixtureOn(ctx, "standard", a.gh, false)
			reqA := a.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
			reqB := b.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
			Expect(a.branch("fix-typo")).NotTo(Equal(b.branch("fix-typo")))

			rowA, err := a.svc.Approve(ctx, a.org, mustID(reqA.ID), a.admin, reqA.ArgsSHA256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(rowA.Status).To(Equal(store.ApprovalStatusExecuted), rowA.Error.String)
			rowB, err := b.svc.Approve(ctx, b.org, mustID(reqB.ID), b.admin, reqB.ArgsSHA256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(rowB.Status).To(Equal(store.ApprovalStatusExecuted), rowB.Error.String)
			Expect(a.gh.Snapshot("widgets").Pulls).To(HaveLen(2))
			Expect(a.gh.Snapshot("widgets").Refs).To(And(HaveKey(a.branch("fix-typo")), HaveKey(b.branch("fix-typo"))))
		})
	})

	It("expires a request nobody decided and cancels its parked run with a task event", func(ctx SpecContext) {
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
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateSubmitted))
		evs := f.taskEvents(ctx)
		Expect(kinds(evs)).To(Equal([]string{approvals.KindRequested, approvals.KindExpired, "run.canceled"}))
		Expect(string(evs[2].Payload)).To(ContainSubstring(`approval_expired`))
		Expect(f.decisions(ctx)).To(Equal([]string{"approval.expire system koinon-api:approvals"}))

		By("an expired request cannot be approved")
		_, err = f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).To(MatchError(approvals.ErrNotPending))
		Expect(err).To(MatchError(approvals.ErrExpired))
		Expect(f.gh.Mints()).To(BeZero())
	})

	It("refuses a decision once expires_at has passed, before the sweeper marked the request, and names the expiry (issue #659)", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		f.park(ctx)
		var at time.Time
		Expect(pool.QueryRow(ctx, `UPDATE approval_requests SET expires_at = now() - interval '1 minute' WHERE id = $1 RETURNING expires_at`,
			mustID(req.ID)).Scan(&at)).To(Succeed())

		_, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "too late")
		Expect(err).To(MatchError(approvals.ErrExpired))
		Expect(err).To(MatchError(approvals.ErrNotPending), "callers that check ErrNotPending still see it")
		Expect(err.Error()).To(ContainSubstring("expired at " + at.UTC().Format(time.RFC3339)))
		_, err = f.svc.Deny(ctx, f.org, mustID(req.ID), f.admin, "too late")
		Expect(err).To(MatchError(approvals.ErrExpired))

		By("nothing was decided, audited or executed")
		row := f.get(ctx, req.ID)
		Expect(row.Status).To(Equal(store.ApprovalStatusPending))
		Expect(row.DecidedBy.Valid).To(BeFalse())
		Expect(f.decisions(ctx)).To(BeEmpty())
		Expect(kinds(f.taskEvents(ctx))).To(Equal([]string{approvals.KindRequested}))

		By("the executor leaves it untouched: only an approved request is claimed")
		got, err := f.svc.Execute(ctx, f.org, mustID(req.ID))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusPending))
		Expect(got.Executions).To(BeZero())
		Expect(f.gh.Mints()).To(BeZero())
	})

	It("does not cancel a run parked on an open question when its approval expires (ADR 0065 TK3)", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		_, err := q.ParkRun(ctx, store.ParkRunParams{
			OrgID: f.org, ID: f.run.ID, Epoch: f.run.Epoch, ParkReason: store.RunParkReasonInputRequired,
			ParkExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
			OrgID: f.org, ID: f.task.ID, ToState: store.TaskStateInputRequired,
			FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking)},
		})
		Expect(err).NotTo(HaveOccurred())
		qrow, _, err := questions.Create(ctx, q, questions.Ask{
			Org: f.org, Task: f.task.ID, Run: f.run.ID, Agent: f.agent.ID,
			Target: questions.Target{Kind: store.NotifyTargetKindUser, User: f.owner}, Recipients: []pgtype.UUID{f.owner},
			Question: "Which region?", Default: "eu-west-1", ExpiresAt: time.Now().Add(24 * time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.OpenAskedQuestionsForRun(ctx, store.OpenAskedQuestionsForRunParams{OrgID: f.org, RunID: f.run.ID})
		Expect(err).NotTo(HaveOccurred())

		_, err = pool.Exec(ctx, `UPDATE approval_requests SET expires_at = now() - interval '1 minute' WHERE id = $1`, mustID(req.ID))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.svc.Sweep(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusExpired))
		Expect(f.getRun(ctx).State).To(Equal(store.RunStateParked), "the open question keeps the run parked")
		Expect(f.getTask(ctx).State).To(Equal(store.TaskStateInputRequired))

		By("answering the question resumes the run, which is not canceled")
		_, err = audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
			if _, err := questions.Respond(ctx, q, b, f.org, qrow.ID, f.owner, questions.Answer{Kind: store.QuestionAnswerKindText, Text: "us-east-1"}); err != nil {
				return err
			}
			return runpark.Resolve(ctx, q, f.org, f.run.ID)
		})
		Expect(err).NotTo(HaveOccurred())
		run := f.getRun(ctx)
		Expect(run.State).To(Equal(store.RunStateParked))
		Expect(run.ReadyAt.Valid).To(BeTrue(), "ready to be claimed")
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
