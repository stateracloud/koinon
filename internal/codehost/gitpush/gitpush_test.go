// SPDX-License-Identifier: Apache-2.0

package gitpush_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

func TestGitPush(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Git Push Writer Suite")
}

const token = "koinon_test_not-a-real-token" //nolint:gosec // a fake, obviously

// reads is the provider side of the Access: only what the writer reads.
type reads struct {
	codehost.Access
	tok string
}

func (r reads) Token() codehost.Token { return codehost.NewToken(r.tok, time.Time{}, nil) }
func (reads) Repository(context.Context, codehost.Repo) (codehost.Repository, error) {
	return codehost.Repository{FullName: "acme/app", DefaultBranch: "main"}, nil
}

var _ = Describe("the push writer", func() {
	var (
		f    *fakegitea.Fake
		repo = codehost.Repo{Owner: "acme", Name: "app"}
		acc  func(tok string) codehost.Access
		base string
		ctx  = context.Background()
	)
	git := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", f.RepoDir("app")}, args...)...).CombinedOutput()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(out))
		return strings.TrimSpace(string(out))
	}

	BeforeEach(func() {
		f = fakegitea.New()
		DeferCleanup(f.Close)
		f.AddToken(token, "koinon-bot")
		f.AddRepo("app")
		base = f.Snapshot("app").Branches["main"]
		acc = func(tok string) codehost.Access {
			return gitpush.New(reads{tok: tok}, gitpush.Options{
				RemoteURL: func(r codehost.Repo) string { return f.URL() + "/" + r.String() + ".git" },
				Now:       func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
			})
		}
	})

	msg := "Add tool\n\nKoinon-Approval: 0192a0b0-0000-7000-8000-000000000001\n"
	files := []codehost.FileWrite{
		{Path: "bin/run.sh", Mode: "100755", Content: []byte("#!/bin/sh\necho hi\n")},
		{Path: "docs/deep/a.md", Mode: "100644", Content: []byte("a\n")},
		{Path: "link", Mode: "120000", Content: []byte("docs/deep/a.md")},
	}

	It("lands a new executable as 100755, with exact modes and the message untouched", func() {
		a := acc(token)
		sha, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: msg, Files: files})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "koinon/add-tool", sha)).To(Succeed())

		Expect(f.Snapshot("app").Branches).To(HaveKeyWithValue("koinon/add-tool", sha))
		Expect(git("rev-parse", sha+"^")).To(Equal(base))
		tree := git("ls-tree", "-r", sha)
		Expect(tree).To(ContainSubstring("100755 blob"))
		Expect(tree).To(MatchRegexp(`100755 blob \w+\tbin/run.sh`))
		Expect(tree).To(MatchRegexp(`100644 blob \w+\tdocs/deep/a.md`))
		Expect(tree).To(MatchRegexp(`120000 blob \w+\tlink`))
		Expect(tree).To(MatchRegexp(`100644 blob \w+\tREADME.md`))
		Expect(git("cat-file", "commit", sha)).To(HaveSuffix("\n\n" + msg[:len(msg)-1]))
		Expect(git("fsck", "--no-dangling")).NotTo(ContainSubstring("error"))
	})

	It("deletes files and prunes empty directories", func() {
		a := acc(token)
		first, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: msg, Files: files})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "step1", first)).To(Succeed())
		second, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: first, Message: "rm\n", Files: []codehost.FileWrite{
			{Path: "docs/deep/a.md", Delete: true},
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "step2", second)).To(Succeed())
		Expect(git("ls-tree", "-r", "--name-only", second)).NotTo(ContainSubstring("docs"))
	})

	It("on replay finds the same commit and the branch stays untouched", func() {
		a := acc(token)
		req := codehost.CommitRequest{Base: base, Message: msg, Files: files}
		sha, err := a.WriteCommit(ctx, repo, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "koinon/add-tool", sha)).To(Succeed())

		b := acc(token)
		again, err := b.WriteCommit(ctx, repo, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(sha))
		Expect(b.CreateBranch(ctx, repo, "koinon/add-tool", again)).To(MatchError(codehost.ErrBranchExists))
		Expect(f.Snapshot("app").Branches).To(HaveKeyWithValue("koinon/add-tool", sha))
	})

	It("never pushes the default branch or an existing branch", func() {
		a := acc(token)
		sha, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: msg, Files: files})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "main", sha)).To(MatchError(gitpush.ErrProtectedBranch))
		Expect(a.CreateBranch(ctx, repo, "refs/heads/main", sha)).To(MatchError(gitpush.ErrProtectedBranch))
		Expect(a.CreateBranch(ctx, repo, "refs/tags/v1", sha)).To(MatchError(gitpush.ErrProtectedBranch))
		Expect(a.CreateBranch(ctx, repo, "HEAD", sha)).To(MatchError(gitpush.ErrProtectedBranch))
		Expect(f.Snapshot("app").Branches).To(Equal(map[string]string{"main": base}))

		Expect(a.CreateBranch(ctx, repo, "feature", sha)).To(Succeed())
		other, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "other\n", Files: files[:1]})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.CreateBranch(ctx, repo, "feature", other)).To(MatchError(codehost.ErrBranchExists))
		Expect(f.Snapshot("app").Branches).To(HaveKeyWithValue("feature", sha))
	})

	It("refuses a commit it did not write, bad modes and bad paths", func() {
		a := acc(token)
		Expect(a.CreateBranch(ctx, repo, "x", base)).To(MatchError(gitpush.ErrUnknownCommit))
		for _, bad := range []codehost.FileWrite{
			{Path: "a", Mode: "100666"}, {Path: "../a", Mode: "100644"}, {Path: ".git/config", Mode: "100644"}, {Path: "/a", Mode: "100644"},
		} {
			_, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: msg, Files: []codehost.FileWrite{bad}})
			Expect(err).To(HaveOccurred(), bad.Path)
		}
	})

	It("does not leak the token when the host refuses it", func() {
		a := acc("koinon_test_wrong-token")
		_, err := a.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: msg, Files: files})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("koinon_test_wrong-token"))
	})
})
