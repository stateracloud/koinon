// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
)

var _ = Describe("Host", func() {
	var (
		fake *githubtest.Fake
		host codehost.Provider
		conn codehost.Connection
	)
	BeforeEach(func() {
		fake = githubtest.New()
		DeferCleanup(fake.Server.Close)
		host = providers.New(github.NewClient(github.Options{BaseURL: fake.URL()})).MustGet(github.HostID)
		cfg, err := json.Marshal(github.Config{AppID: fake.AppID, InstallationID: fake.InstallationID})
		Expect(err).NotTo(HaveOccurred())
		conn = codehost.Connection{ID: "c1", Connector: github.HostID, Config: cfg, Fields: map[string][]byte{github.PrivateKeyField: fake.PEM}}
	})

	It("is the only provider of the default registry and describes its credential", func() {
		Expect(providers.Default().IDs()).To(Equal([]string{"github"}))
		caps := host.Capabilities()
		Expect(caps.DisplayName).To(Equal("GitHub"))
		Expect(caps.NarrowsRepositories).To(BeTrue())
		Expect(caps.CIPaths).To(ConsistOf(".github/workflows/"))
	})

	It("implements every check the GitHub setup guide names", func() {
		ct, ok := connectors.Get(github.HostID)
		Expect(ok).To(BeTrue())
		for _, step := range ct.Spec.SetupGuide.Steps {
			if step.Check != "" {
				Expect(host.CheckNames()).To(ContainElement(step.Check))
			}
		}
	})

	It("writes a commit with exact modes, a branch and a pull request, and reads them back", func(ctx SpecContext) {
		repo, err := host.ParseRepo("acme/widgets")
		Expect(err).NotTo(HaveOccurred())
		acc, err := host.Open(ctx, conn, codehost.Scope{
			Repositories: []string{repo.String()},
			Permissions:  map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(acc.Token().Value()).NotTo(BeEmpty())
		Expect(acc.Token().String()).NotTo(ContainSubstring(acc.Token().Value()))

		r, err := acc.Repository(ctx, repo)
		Expect(err).NotTo(HaveOccurred())
		base, found, err := acc.Branch(ctx, repo, r.DefaultBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())

		sha, err := acc.WriteCommit(ctx, repo, codehost.CommitRequest{Base: base, Message: "add tool\n", Files: []codehost.FileWrite{
			{Path: "bin/tool.sh", Mode: "100755", Content: []byte("#!/bin/sh\n")},
			{Path: "README.md", Mode: "100644", Content: []byte("hi\n")},
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(acc.CreateBranch(ctx, repo, "feature", sha)).To(Succeed())
		Expect(acc.CreateBranch(ctx, repo, "feature", sha)).To(MatchError(codehost.ErrBranchExists))

		modes, err := acc.Modes(ctx, repo, sha, []string{"bin/tool.sh", "README.md", "new.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(modes).To(Equal(map[string]string{"bin/tool.sh": "100755", "README.md": "100644"}))
		c, err := acc.Commit(ctx, repo, sha)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Message).To(Equal("add tool\n"))
		Expect(c.Parents).To(Equal([]string{base}))

		pr, err := acc.CreatePullRequest(ctx, repo, codehost.PullRequestInput{Title: "t", Body: "b", Head: "feature", Base: r.DefaultBranch})
		Expect(err).NotTo(HaveOccurred())
		prs, err := acc.PullRequests(ctx, repo, "feature")
		Expect(err).NotTo(HaveOccurred())
		Expect(prs).To(HaveLen(1))
		Expect(prs[0].Number).To(Equal(pr.Number))
		Expect(prs[0].HeadRef).To(Equal("feature"))
		Expect(host.BranchURL(repo, "feature")).To(Equal("https://github.com/acme/widgets/tree/feature"))
	})

	It("reports an API status through codehost.Status", func(ctx SpecContext) {
		repo := codehost.Repo{Owner: "acme", Name: "widgets"}
		acc, err := host.Open(ctx, conn, codehost.Scope{Repositories: []string{repo.String()}, Permissions: map[string]string{"contents": "read"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = acc.Commit(ctx, repo, "0000000000000000000000000000000000000000")
		Expect(codehost.Status(err)).To(BeNumerically(">=", 400), "%v", err)
	})

	It("refuses a connection of another connector", func(ctx SpecContext) {
		conn.Connector = "gitea"
		_, err := host.Open(ctx, conn, host.MetadataScope(nil))
		Expect(err).To(MatchError(ContainSubstring("not GitHub")))
	})
})
