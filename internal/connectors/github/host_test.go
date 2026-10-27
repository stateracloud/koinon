// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"time"

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
		Expect(caps.CredentialClass).To(Equal("app-installation"))
		Expect(caps.TokenLifetime).To(Equal(time.Hour))
		Expect(caps.MaxRepositories).To(Equal(500))
		Expect(caps.CanDenyCI).To(BeTrue())
		Expect(caps.ExactModes).To(BeTrue())
	})

	Describe("on a host with its own CA", func() {
		// A GitHub Enterprise Server stand-in: TLS with a certificate no system root signs,
		// and the REST API under /api/v3 of the same host.
		var (
			srv  *httptest.Server
			ca   string
			ghes codehost.Provider
		)
		BeforeEach(func() {
			srv = httptest.NewTLSServer(http.StripPrefix("/api/v3", fake.Handler))
			DeferCleanup(srv.Close)
			ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
			ghes = github.NewHost(github.NewClient(github.Options{}))
		})
		connAt := func(site codehost.Site) codehost.Connection {
			cfg, err := json.Marshal(github.Config{AppID: fake.AppID, InstallationID: fake.InstallationID, Site: site})
			Expect(err).NotTo(HaveOccurred())
			return codehost.Connection{ID: "c2", Connector: github.HostID, Config: cfg, Fields: map[string][]byte{github.PrivateKeyField: fake.PEM}}
		}

		It("calls the API at the connection's host, trusting its CA bundle", func(ctx SpecContext) {
			conn := connAt(codehost.Site{BaseURL: srv.URL, CABundle: ca})
			repo := codehost.Repo{Owner: "acme", Name: "widgets"}
			acc, err := ghes.Open(ctx, conn, codehost.Scope{Repositories: []string{repo.String()}, Permissions: map[string]string{"contents": "read"}})
			Expect(err).NotTo(HaveOccurred())
			got, err := acc.Repository(ctx, repo)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.FullName).To(Equal("acme/widgets"))
			Expect(fake.Mints()).To(Equal(1))
			Expect(ghes.BranchURL(conn, repo, "feature")).To(Equal(srv.URL + "/acme/widgets/tree/feature"))

			// The setup guide's checks run against the same host.
			ct, _ := connectors.Get(github.HostID)
			for _, r := range ghes.Checks(ctx, conn, ct) {
				Expect(r.OK).To(BeTrue(), "%s: %s", r.Check, r.Message)
			}
		})

		It("fails the TLS handshake without the CA bundle", func(ctx SpecContext) {
			_, err := ghes.Open(ctx, connAt(codehost.Site{BaseURL: srv.URL}), ghes.MetadataScope(nil))
			Expect(err).To(MatchError(ContainSubstring("certificate")))
		})

		It("rejects a CA bundle that holds a private key", func(ctx SpecContext) {
			bad := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not-a-real-key")}))
			_, err := ghes.Open(ctx, connAt(codehost.Site{BaseURL: srv.URL, CABundle: bad}), ghes.MetadataScope(nil))
			Expect(err).To(MatchError(ContainSubstring("only CERTIFICATE blocks")))
		})

		It("refuses a base URL that is not a bare https host", func(ctx SpecContext) {
			_, err := ghes.Open(ctx, connAt(codehost.Site{BaseURL: "http://ghes.example.test"}), ghes.MetadataScope(nil))
			Expect(err).To(MatchError(ContainSubstring("https")))
		})
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
		Expect(host.BranchURL(codehost.Connection{}, repo, "feature")).To(Equal("https://github.com/acme/widgets/tree/feature"))
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
