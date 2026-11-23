// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
)

// The review request (issue #1175): after the App opens an agent pull request, Koinon asks the
// requester to review it when the requester linked their login on that GitHub host.
var _ = Describe("Review request", func() {
	link := func(ctx context.Context, f *fixture, user pgtype.UUID, host, login string, id int64) {
		GinkgoHelper()
		_, err := q.UpsertGitHubUserLink(ctx, store.UpsertGitHubUserLinkParams{
			OrgID: f.org, UserID: user, Host: host, GithubUserID: id, GithubLogin: login,
		})
		Expect(err).NotTo(HaveOccurred())
	}
	approve := func(ctx context.Context, f *fixture) store.ApprovalRequest {
		GinkgoHelper()
		req := f.call(ctx, githubtools.ToolCreatePullRequest, prArgs())
		row, err := f.svc.Approve(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		return row
	}

	It("asks the linked requester to review the pull request", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		link(ctx, f, f.owner, f.gh.WebURL(), "olive-gh", 101)
		link(ctx, f, f.admin, f.gh.WebURL(), "adam-gh", 102)

		row := approve(ctx, f)
		Expect(result(row)).To(HaveKeyWithValue("reviewRequested", "olive-gh"))
		Expect(f.gh.Reviewers("widgets", 1)).To(Equal([]string{"olive-gh"}), "the requester, not the approver")
	})

	It("asks nobody when the requester has no link", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		link(ctx, f, f.admin, f.gh.WebURL(), "adam-gh", 102)

		row := approve(ctx, f)
		Expect(result(row)).NotTo(HaveKey("reviewRequested"))
		Expect(f.gh.Reviewers("widgets", 1)).To(BeEmpty())
		Expect(f.gh.Snapshot("widgets").Calls).NotTo(HaveKey("POST pulls/{number}/requested_reviewers"))
	})

	It("uses a link only on its own GitHub host", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		link(ctx, f, f.owner, "https://github.example.test", "olive-ghes", 103)

		row := approve(ctx, f)
		Expect(result(row)).NotTo(HaveKey("reviewRequested"))
		Expect(f.gh.Reviewers("widgets", 1)).To(BeEmpty())
	})

	It("uses the link of the GitHub Enterprise Server host for a connection to it", func(ctx SpecContext) {
		f := newGHESFixture(ctx, "standard")
		link(ctx, f, f.owner, "https://github.com", "olive-dotcom", 104)
		link(ctx, f, f.owner, f.gh.WebURL(), "olive-ghes", 105)

		row := approve(ctx, f)
		Expect(result(row)).To(HaveKeyWithValue("reviewRequested", "olive-ghes"))
		Expect(f.gh.Reviewers("widgets", 1)).To(Equal([]string{"olive-ghes"}))
	})

	It("keeps the pull request when GitHub refuses the review request", func(ctx SpecContext) {
		f := newFixture(ctx, "standard")
		f.gh.Collaborators = []string{"someone-else"} // the requester is not a collaborator: 422
		link(ctx, f, f.owner, f.gh.WebURL(), "olive-gh", 101)

		row := approve(ctx, f)
		Expect(result(row)).To(HaveKeyWithValue("url", "https://github.test/acme/widgets/pull/1"))
		Expect(result(row)).NotTo(HaveKey("reviewRequested"))
		Expect(f.gh.Snapshot("widgets").Pulls).To(HaveLen(1))
		Expect(f.gh.Reviewers("widgets", 1)).To(BeEmpty())
		Expect(f.logs.String()).To(ContainSubstring("the review request failed"))
	})
})
