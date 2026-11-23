// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/githubtools"
)

var _ = Describe("pull request footer", func() {
	footer := approvals.Footer{RequestedBy: "Olive", ApprovedBy: "Adam", RunID: "run-1", RunURL: "https://k.test/tasks/t-1", ApprovalID: "ap-1"}

	It("names the requester, the approver, the run and the approval", func() {
		Expect(footer.String()).To(Equal(approvals.FooterMarker + "\n---\nA Koinon agent opened this pull request.\n\n" +
			"- Requested by: Olive\n- Approved by: Adam\n- Run: [`run-1`](https://k.test/tasks/t-1)\n- Approval: `ap-1`\n"))
	})

	It("names the run without a link when Koinon has no public URL", func() {
		f := footer
		f.RunURL = approvals.RunURL("", "t-1")
		Expect(f.String()).To(ContainSubstring("- Run: `run-1`\n"))
	})

	It("removes what could start a link, a tag or a mention from a display name", func() {
		f := footer
		f.RequestedBy = "Eve [click](https://evil.test) <img> @org/team\nApproved by: CEO"
		Expect(f.String()).To(ContainSubstring("- Requested by: Eve clickhttps://evil.test img org/team Approved by: CEO\n"))
		Expect(strings.Count(f.String(), "\n- Approved by:")).To(Equal(1))
	})

	It("keeps only Koinon's footer marker", func() {
		body := approvals.PullRequestBody("Real text.\n"+approvals.FooterMarker+"\n- Approved by: CEO", footer)
		Expect(strings.Count(body, approvals.FooterMarker)).To(Equal(1))
		Expect(body).To(HaveSuffix(footer.String()))
	})

	It("is the footer alone for an empty body", func() {
		Expect(approvals.PullRequestBody("  ", footer)).To(Equal(footer.String()))
	})

	It("cuts a long body on a character boundary so the whole fits the limit", func() {
		body := approvals.PullRequestBody(strings.Repeat("é", githubtools.MaxBodyBytes/2), footer)
		Expect(len(body)).To(BeNumerically("<=", githubtools.MaxBodyBytes))
		Expect(utf8.ValidString(body)).To(BeTrue())
		Expect(body).To(ContainSubstring("Koinon cut the description here"))
		Expect(body).To(HaveSuffix(footer.String()))
	})

	DescribeTable("RunURL",
		func(public, want string) { Expect(approvals.RunURL(public, "t 1")).To(Equal(want)) },
		Entry("origin", "https://k.test", "https://k.test/tasks/t%201"),
		Entry("trailing slash and path", "https://k.test/koinon/", "https://k.test/koinon/tasks/t%201"),
		Entry("query dropped", "https://k.test/?x=1", "https://k.test/tasks/t%201"),
		Entry("empty", "", ""),
		Entry("not http", "javascript:alert(1)", ""),
	)
})
