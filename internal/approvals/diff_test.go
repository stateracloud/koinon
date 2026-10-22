// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/approvals"
	"github.com/procoduck/koinon/internal/connectors/github/githubtest"
	"github.com/procoduck/koinon/internal/githubtools"
)

func file(path, content string) map[string]any {
	return map[string]any{"path": path, "content": content}
}

func diffArgs(files ...map[string]any) map[string]any {
	return map[string]any{"branch": "koinon/diff", "title": "Change files", "files": files}
}

var _ = Describe("Diff", func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx, "standard")
		f.gh.SetBranch("widgets", "main", "seed", map[string]string{
			"docs/guide.md": "# Guide\n\none\ntwo\nthree\n",
			"old.txt":       "obsolete\n",
			"same.txt":      "unchanged\n",
			"logo.png":      "\x89PNG\r\n\x1a\n\x00\x00",
		})
	})

	It("compares each file with the head of the base branch, with a read-only token", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCreatePullRequest, diffArgs(
			file("docs/guide.md", "# Guide\n\none\n2\nthree\n"),
			file("new.txt", "hello\nworld"),
			map[string]any{"path": "old.txt", "delete": true},
			file("same.txt", "unchanged\n"),
			file("logo.png", "\x89PNG\r\n\x1a\n\x00\x01"),
		))
		mints := f.gh.Mints()

		d, err := f.svc.Diff(ctx, f.get(ctx, req.ID))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Base).To(Equal("main"))
		Expect(d.Files).To(Equal([]approvals.FileDiff{
			{Path: "docs/guide.md", Status: approvals.StatusModified, Diff: "--- a/docs/guide.md\n+++ b/docs/guide.md\n" +
				"@@ -1,5 +1,5 @@\n # Guide\n \n one\n-two\n+2\n three\n"},
			{Path: "logo.png", Status: approvals.StatusModified, Binary: true},
			{Path: "new.txt", Status: approvals.StatusAdded, Diff: "--- /dev/null\n+++ b/new.txt\n" +
				"@@ -0,0 +1,2 @@\n+hello\n+world\n\\ No newline at end of file\n"},
			{Path: "old.txt", Status: approvals.StatusDeleted, Diff: "--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-obsolete\n"},
			{Path: "same.txt", Status: approvals.StatusModified},
		}), "one entry per file, in the recorded (sorted) order")

		By("one token, for the one repository, that can only read")
		Expect(f.gh.Mints()).To(Equal(mints + 1))
		reqs := f.gh.Requests()
		Expect(reqs[len(reqs)-1]).To(Equal(githubtest.Request{
			Repositories: []string{"widgets"}, Permissions: map[string]string{"metadata": "read", "contents": "read"},
		}))
		Expect(f.gh.Snapshot("widgets").Calls).To(BeEmpty(), "nothing was written")
	})

	It("falls back per file when GitHub fails or the current version is too large", func(ctx SpecContext) {
		f.gh.SetBranch("widgets", "main", "big", map[string]string{"big.txt": strings.Repeat("x\n", 600_000)})
		f.gh.FailContents("widgets", "docs/guide.md", http.StatusBadGateway)
		req := f.call(ctx, githubtools.ToolCreatePullRequest, diffArgs(
			file("docs/guide.md", "new\n"),
			file("big.txt", "small now\n"),
			file("same.txt", "changed\n"),
		))

		d, err := f.svc.Diff(ctx, f.get(ctx, req.ID))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Files).To(HaveLen(3))
		Expect(d.Files[0]).To(Equal(approvals.FileDiff{Path: "big.txt", Fallback: "the current version is larger than 1 MiB (1200000 bytes)"}))
		Expect(d.Files[1]).To(Equal(approvals.FileDiff{
			Path: "docs/guide.md", Fallback: "GitHub answered 502 Bad Gateway when Koinon read the current version",
		}))
		Expect(d.Files[2].Diff).To(ContainSubstring("-unchanged\n+changed\n"))
	})

	It("falls back for every file when the base branch does not exist", func(ctx SpecContext) {
		args := diffArgs(file("docs/guide.md", "new\n"), map[string]any{"path": "old.txt", "delete": true})
		args["base"] = "release"
		req := f.call(ctx, githubtools.ToolCreatePullRequest, args)

		d, err := f.svc.Diff(ctx, f.get(ctx, req.ID))
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(approvals.Diff{Base: "release", Files: []approvals.FileDiff{
			{Path: "docs/guide.md", Fallback: "the base branch release does not exist in acme/widgets"},
			{Path: "old.txt", Status: approvals.StatusDeleted, Fallback: "the base branch release does not exist in acme/widgets"},
		}}))
	})

	It("has no diff for a comment", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 7, "body": "hi"})
		_, err := f.svc.Diff(ctx, f.get(ctx, req.ID))
		Expect(err).To(MatchError(approvals.ErrNoDiff))
	})
})

var _ = DescribeTable("UnifiedDiff",
	func(old, new string, status approvals.FileStatus, want string) {
		Expect(approvals.UnifiedDiff("f.txt", old, new, status)).To(Equal(want))
	},
	Entry("equal contents", "a\n", "a\n", approvals.StatusModified, ""),
	Entry("a missing last newline is a change", "a\n", "a", approvals.StatusModified,
		"--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-a\n+a\n\\ No newline at end of file\n"),
	Entry("CRLF line endings are kept", "a\r\nb\r\n", "a\r\nc\r\n", approvals.StatusModified,
		"--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n a\r\n-b\r\n+c\r\n"),
	Entry("an added empty file", "", "", approvals.StatusAdded, ""),
)
