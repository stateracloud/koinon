// SPDX-License-Identifier: Apache-2.0

package githubtools_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	approvalv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/procoduck/koinon/internal/githubtools"
)

var _ = Describe("WriteTool", func() {
	It("describes create_pull_request and comment_on_issue", func() {
		pr, err := githubtools.LookupWriteTool(githubtools.ToolCreatePullRequest)
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.Scope).To(Equal("pull_request.create"))
		Expect(pr.Permissions).To(Equal(map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"}))

		c, err := githubtools.LookupWriteTool(githubtools.ToolCommentOnIssue)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Scope).To(Equal("issue.comment"))
		Expect(c.Permissions).To(Equal(map[string]string{"metadata": "read", "issues": "write"}))
	})

	It("treats any other tool as unknown, including the read-only status tool", func() {
		for _, name := range []string{"delete_repository", githubtools.ToolGetApprovalStatus, ""} {
			_, err := githubtools.LookupWriteTool(name)
			var unknown *githubtools.UnknownToolError
			Expect(err).To(BeAssignableToTypeOf(unknown), name)
			_, err = githubtools.DecodeCall(name, []byte(`{"issue_number": 7, "body": "x"}`))
			Expect(err).To(MatchError(ContainSubstring("unknown GitHub write tool")), name)
		}
	})

	It("decodes, validates and converts a pull request call", func() {
		raw, err := json.Marshal(validPR())
		Expect(err).NotTo(HaveOccurred())
		call, err := githubtools.DecodeCall(githubtools.ToolCreatePullRequest, raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(call.Tool()).To(Equal(githubtools.ToolCreatePullRequest))
		args, ok := call.(*githubtools.PullRequestArgs)
		Expect(ok).To(BeTrue())
		Expect(*args).To(Equal(validPR()))

		out := &approvalv1alpha1.Approval{}
		call.SetApprovalCall(out)
		Expect(out.GetPullRequest().GetBranch()).To(Equal("koinon/fix-typo"))
		Expect(out.GetPullRequest().GetFiles()).To(HaveLen(1))
		Expect(out.GetPullRequest().GetFiles()[0].GetPath()).To(Equal("README.md"))
	})

	It("decodes, validates and converts a comment call", func() {
		call, err := githubtools.DecodeCall(githubtools.ToolCommentOnIssue, []byte(`{"issue_number": 7, "body": "Opened a PR."}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(call).To(Equal(&githubtools.CommentArgs{IssueNumber: 7, Body: "Opened a PR."}))
		out := &approvalv1alpha1.Approval{}
		call.SetApprovalCall(out)
		Expect(out.GetComment().GetIssueNumber()).To(BeEquivalentTo(7))
	})

	It("refuses arguments that do not decode or do not validate", func() {
		_, err := githubtools.DecodeCall(githubtools.ToolCommentOnIssue, []byte(`{"issue_number": "seven"}`))
		Expect(err).To(MatchError(ContainSubstring("decode the recorded arguments")))
		_, err = githubtools.DecodeCall(githubtools.ToolCommentOnIssue, []byte(`{"issue_number": 0, "body": "x"}`))
		var refused *githubtools.ValidationError
		Expect(err).To(BeAssignableToTypeOf(refused))
		_, err = githubtools.DecodeCall(githubtools.ToolCreatePullRequest, []byte(`{"branch": "main", "title": "x", "files": []}`))
		Expect(err).To(BeAssignableToTypeOf(refused))
	})
})
