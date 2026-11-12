// SPDX-License-Identifier: Apache-2.0

package gitea_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

// Obviously fake tokens (AGENTS.md).
const (
	readTok  = "koinon_test_not-a-real-read-token"
	writeTok = "koinon_test_not-a-real-write-token"
)

type env struct {
	fake *fakegitea.Fake
	host *gitea.Host
	conn codehost.Connection
}

// newEnv starts a TLS fake with two repositories and a bot (collaborator on both) with a read and
// a write token; mutate adjusts the fake before the connection is built.
func newEnv(mutate func(*fakegitea.Fake)) *env {
	f := fakegitea.NewTLS()
	DeferCleanup(f.Close)
	f.Tokens = map[string]string{}
	f.AddScopedToken(readTok, "koinon-bot", "read")
	f.AddScopedToken(writeTok, "koinon-bot", "write")
	f.AddRepo("app")
	f.AddRepo("lib")
	f.AddRepo("other")
	f.Collaborate("koinon-bot", "app", "write")
	f.Collaborate("koinon-bot", "lib", "write")
	if mutate != nil {
		mutate(f)
	}
	return &env{fake: f, host: gitea.NewHost(gitea.Options{}), conn: connection(f.URL(), f.CAPEM())}
}

func connection(base, ca string) codehost.Connection {
	cfg := gitea.NewConfig(codehost.Site{BaseURL: base, CABundle: ca}, []byte(readTok), []byte(writeTok), nil, time.Now())
	raw, err := json.Marshal(cfg)
	Expect(err).NotTo(HaveOccurred())
	return codehost.Connection{
		ID: "c1", DisplayName: "git", Connector: gitea.HostID, Config: raw,
		Fields: map[string][]byte{gitea.ReadTokenField: []byte(readTok), gitea.WriteTokenField: []byte(writeTok)},
	}
}

var (
	read  = codehost.Scope{Permissions: map[string]string{"repository": "read", "issue": "read"}}
	write = codehost.Scope{Permissions: map[string]string{"repository": "write"}}
)

func giteaType() *connectors.Type {
	t, ok := connectors.Get(gitea.HostID)
	Expect(ok).To(BeTrue())
	return t
}

var _ = Describe("the Gitea provider", func() {
	ctx := context.Background()
	app := codehost.Repo{Owner: "acme", Name: "app"}

	It("is registered, with the weaker-credential descriptor from the connector data", func() {
		p := providers.Default().MustGet("gitea")
		caps := p.Capabilities()
		Expect(caps.DisplayName).To(Equal("Gitea"))
		Expect(caps.CredentialClass).To(Equal("static-token"))
		Expect(caps.NarrowsRepositories).To(BeFalse())
		Expect(caps.CanDenyCI).To(BeFalse())
		Expect(caps.Writes).To(Equal(codehost.WritesPush))
		Expect(caps.TokenLifetime).To(BeZero())
		Expect(caps.TokenWarnAfter).To(Equal(90 * 24 * time.Hour))
		Expect(caps.CIPaths).To(ConsistOf(".gitea/workflows/", ".gitea/scoped_workflows/", ".github/workflows/"))
	})

	Describe("credentials", func() {
		It("opens the read token for a read scope and the write token for any write", func() {
			e := newEnv(nil)
			r, err := e.host.Open(ctx, e.conn, read)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Token().Value()).To(Equal(readTok))
			Expect(r.Token().ExpiresAt).To(BeZero())
			Expect(r.Token().Revision).To(HavePrefix("gitea-read-"))
			Expect(r.Token().Revision).NotTo(ContainSubstring(readTok))
			w, err := e.host.Open(ctx, e.conn, write)
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Token().Value()).To(Equal(writeTok))
			meta, err := e.host.Open(ctx, e.conn, e.host.MetadataScope([]string{"acme/app"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Token().Value()).To(Equal(readTok), "naming repositories is a read")
		})

		It("never prints a token", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, write)
			Expect(a.Token().String()).NotTo(ContainSubstring("write-token"))
			Expect(a.Token().GoString()).NotTo(ContainSubstring("write-token"))
		})

		It("changes the read revision with the token", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, read)
			e.conn.Fields[gitea.ReadTokenField] = []byte("koinon_test_not-a-real-read-token-2")
			b, _ := e.host.Open(ctx, e.conn, read)
			Expect(a.Token().Revision).NotTo(Equal(b.Token().Revision))
		})

		DescribeTable("rejects malformed secrets",
			func(read, write string) {
				err := gitea.NewHost(gitea.Options{}).ValidateSecrets(map[string][]byte{gitea.ReadTokenField: []byte(read), gitea.WriteTokenField: []byte(write)})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).NotTo(ContainSubstring("not-a-real"))
			},
			Entry("empty read", "", "koinon_test_not-a-real-w"),
			Entry("empty write", "koinon_test_not-a-real-r", "  "),
			Entry("a space", "koinon_test not-a-real-r", "koinon_test_not-a-real-w"),
			Entry("a line break", "koinon_test_not-a-real-r", "koinon_test_not-a-real-w\n"),
			Entry("the same token twice", "koinon_test_not-a-real-t", "koinon_test_not-a-real-t"),
			Entry("too long", strings.Repeat("a", 300), "koinon_test_not-a-real-w"),
		)

		It("validates a whole connection, including a base URL", func() {
			e := newEnv(nil)
			Expect(e.host.Validate(e.conn)).To(Succeed())
			e.conn.Config = []byte(`{}`)
			Expect(e.host.Validate(e.conn)).To(MatchError(ContainSubstring("needs the URL of your server")))
		})
	})

	Describe("reading", func() {
		It("lists the repositories the bot can see, in pages of the caller's size", func() {
			e := newEnv(nil)
			a, err := e.host.Open(ctx, e.conn, read)
			Expect(err).NotTo(HaveOccurred())
			p1, err := a.Repositories(ctx, 1, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(p1.Total).To(BeEquivalentTo(2), "other is not shared with the bot")
			Expect(p1.Repositories).To(HaveLen(1))
			Expect(p1.Repositories[0].FullName).To(Equal("acme/app"))
			p2, err := a.Repositories(ctx, 2, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(p2.Repositories[0].FullName).To(Equal("acme/lib"))
			all, err := a.Repositories(ctx, 1, codehost.MaxPerPage)
			Expect(err).NotTo(HaveOccurred())
			Expect(all.Repositories).To(HaveLen(2))
		})

		It("reports private only for a private repository, and public when unsure (ADR 0091)", func() {
			e := newEnv(func(f *fakegitea.Fake) {
				f.SetVisibility("lib", "public")
				f.AddRepo("hub")
				f.Collaborate("koinon-bot", "hub", "read")
				f.SetVisibility("hub", "internal") // public in a private organization
			})
			a, err := e.host.Open(ctx, e.conn, read)
			Expect(err).NotTo(HaveOccurred())
			all, err := a.Repositories(ctx, 1, codehost.MaxPerPage)
			Expect(err).NotTo(HaveOccurred())
			got := map[string]string{}
			for _, r := range all.Repositories {
				got[r.FullName] = r.Visibility
			}
			Expect(got).To(Equal(map[string]string{"acme/app": "private", "acme/lib": "public", "acme/hub": "public"}))
			one, err := a.Repository(ctx, app)
			Expect(err).NotTo(HaveOccurred())
			Expect(one.Visibility).To(Equal(codehost.VisibilityPrivate))
		})

		It("pages across Gitea's 50-item pages", func() {
			e := newEnv(func(f *fakegitea.Fake) {
				for i := 0; i < 52; i++ {
					name := "r" + string(rune('a'+i/26)) + string(rune('a'+i%26))
					f.AddRepo(name)
					f.Collaborate("koinon-bot", name, "read")
				}
			})
			a, _ := e.host.Open(ctx, e.conn, read)
			p, err := a.Repositories(ctx, 1, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Total).To(BeEquivalentTo(54))
			Expect(p.Repositories).To(HaveLen(54), "two Gitea pages fill one of 100")
			p2, err := a.Repositories(ctx, 2, 40)
			Expect(err).NotTo(HaveOccurred())
			Expect(p2.Repositories).To(HaveLen(14))
			Expect(p2.Repositories[0].FullName).To(Equal(p.Repositories[40].FullName))
		})

		It("reads a repository, a branch, a commit, a file and the modes of paths", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, read)
			r, err := a.Repository(ctx, app)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.FullName).To(Equal("acme/app"))
			Expect(r.DefaultBranch).To(Equal("main"))
			sha, found, err := a.Branch(ctx, app, "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			_, found, err = a.Branch(ctx, app, "nope")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			c, err := a.Commit(ctx, app, sha)
			Expect(err).NotTo(HaveOccurred())
			Expect(c.SHA).To(Equal(sha))
			f, found, err := a.File(ctx, app, "README.md", "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(f.Type).To(Equal("file"))
			Expect(f.Content).NotTo(BeEmpty())
			_, found, err = a.File(ctx, app, "missing.txt", "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			modes, err := a.Modes(ctx, app, sha, []string{"README.md", "new.txt"})
			Expect(err).NotTo(HaveOccurred())
			Expect(modes).To(Equal(map[string]string{"README.md": "100644"}))
		})

		It("hides a repository the bot is not a collaborator on", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, read)
			_, err := a.Repository(ctx, codehost.Repo{Owner: "acme", Name: "other"})
			Expect(codehost.IsStatus(err, http.StatusNotFound)).To(BeTrue(), "%v", err)
		})
	})

	Describe("writing", func() {
		It("leaves commits and branches to the push writer", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, write)
			_, err := a.WriteCommit(ctx, app, codehost.CommitRequest{})
			Expect(err).To(HaveOccurred())
			Expect(a.CreateBranch(ctx, app, "x", "y")).NotTo(Succeed())
		})

		It("opens a pull request, finds it by branch and comments on it", func() {
			e := newEnv(nil)
			w, err := gitpush.Open(ctx, e.host, e.conn, write, nil)
			Expect(err).NotTo(HaveOccurred())
			base, _, err := w.Branch(ctx, app, "main")
			Expect(err).NotTo(HaveOccurred())
			sha, err := w.WriteCommit(ctx, app, codehost.CommitRequest{
				Base: base, Message: "Add\n\nKoinon-Approval: x\n",
				Files: []codehost.FileWrite{{Path: "bin/run.sh", Mode: "100755", Content: []byte("#!/bin/sh\n")}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(w.CreateBranch(ctx, app, "koinon/add", sha)).To(Succeed())
			Expect(w.CreateBranch(ctx, app, "koinon/add", sha)).To(MatchError(codehost.ErrBranchExists))
			pr, err := w.CreatePullRequest(ctx, app, codehost.PullRequestInput{Title: "Add", Body: "b", Head: "koinon/add", Base: "main"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Number).To(BeEquivalentTo(1))
			Expect(pr.HeadRef).To(Equal("koinon/add"))
			prs, err := w.PullRequests(ctx, app, "koinon/add")
			Expect(err).NotTo(HaveOccurred())
			Expect(prs).To(HaveLen(1))
			none, err := w.PullRequests(ctx, app, "koinon/none")
			Expect(err).NotTo(HaveOccurred())
			Expect(none).To(BeEmpty())
			cm, err := w.CreateComment(ctx, app, pr.Number, "hello")
			Expect(err).NotTo(HaveOccurred())
			cs, err := w.Comments(ctx, app, pr.Number)
			Expect(err).NotTo(HaveOccurred())
			Expect(cs).To(HaveLen(1))
			Expect(cs[0].ID).To(Equal(cm.ID))
			modes, err := w.Modes(ctx, app, sha, []string{"bin/run.sh"})
			Expect(err).NotTo(HaveOccurred())
			Expect(modes).To(HaveKeyWithValue("bin/run.sh", "100755"), "a new executable landed as 100755")
			Expect(e.fake.Snapshot("app").Branches).To(HaveKeyWithValue("koinon/add", sha))
		})

		It("is refused with the read token: no push, no pull request, no comment", func() {
			e := newEnv(nil)
			// The read token behind the push writer, as if the wrong token were opened.
			a, err := e.host.Open(ctx, e.conn, read)
			Expect(err).NotTo(HaveOccurred())
			_, err = a.CreatePullRequest(ctx, app, codehost.PullRequestInput{Title: "x", Head: "h", Base: "main"})
			Expect(codehost.IsStatus(err, http.StatusForbidden)).To(BeTrue(), "%v", err)
			_, err = a.CreateComment(ctx, app, 1, "x")
			Expect(codehost.IsStatus(err, http.StatusForbidden)).To(BeTrue(), "%v", err)
			opts, err := gitpush.ForSite(giteaType(), codehost.Site{BaseURL: e.fake.URL(), CABundle: e.fake.CAPEM()})
			Expect(err).NotTo(HaveOccurred())
			w := gitpush.New(a, opts)
			base, _, _ := a.Branch(ctx, app, "main")
			sha, err := w.WriteCommit(ctx, app, codehost.CommitRequest{Base: base, Message: "m", Files: []codehost.FileWrite{{Path: "a", Mode: "100644", Content: []byte("a")}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(w.CreateBranch(ctx, app, "koinon/x", sha)).NotTo(Succeed())
			Expect(e.fake.Snapshot("app").Branches).NotTo(HaveKey("koinon/x"))
		})

		It("does not put a token into an error", func() {
			e := newEnv(nil)
			a, _ := e.host.Open(ctx, e.conn, write)
			_, err := a.CreatePullRequest(ctx, app, codehost.PullRequestInput{Head: "h", Base: "main"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("not-a-real"))
		})
	})

	Describe("the connection check", func() {
		check := func(e *env) map[string]codehost.CheckResult {
			out := map[string]codehost.CheckResult{}
			for _, r := range e.host.Checks(ctx, e.conn, giteaType()) {
				out[r.Check] = r
			}
			return out
		}

		It("passes and reports what the bot can see, its permission per repository and that it can push", func() {
			e := newEnv(nil)
			got := check(e)
			Expect(got).To(HaveLen(4))
			for _, r := range got {
				Expect(r.OK).To(BeTrue(), "%s: %s", r.Check, r.Message)
			}
			Expect(got["gitea.server"].Message).To(ContainSubstring("Gitea 28.2.0"))
			Expect(got["gitea.tokens"].Message).To(ContainSubstring("koinon-bot"))
			m := got["gitea.repositories"].Message
			Expect(m).To(ContainSubstring("2 repositories and push to 2"))
			Expect(m).To(ContainSubstring("acme/app (write)"))
			Expect(m).To(ContainSubstring("acme/lib (write)"))
			Expect(m).NotTo(ContainSubstring("other"))
			Expect(got["gitea.read-token"].Message).To(ContainSubstring("refused a write"))
			for _, r := range got {
				Expect(r.Message).NotTo(ContainSubstring("not-a-real"))
			}
			Expect(e.fake.Snapshot("app").Calls).NotTo(HaveKey("POST issues"), "nothing is written")
		})

		It("lists the checks in the setup guide's step order", func() {
			e := newEnv(nil)
			var steps []string
			for _, r := range e.host.Checks(ctx, e.conn, giteaType()) {
				steps = append(steps, r.Step)
			}
			Expect(steps).To(Equal([]string{"create-bot", "create-tokens", "test-repositories", "test-read-token"}))
			Expect(e.host.CheckNames()).To(ConsistOf("gitea.server", "gitea.tokens", "gitea.repositories", "gitea.read-token"))
		})

		It("shows read-only and administrator permissions", func() {
			e := newEnv(func(f *fakegitea.Fake) {
				f.Collaborate("koinon-bot", "lib", "read")
				f.Collaborate("koinon-bot", "other", "admin")
			})
			m := check(e)["gitea.repositories"]
			Expect(m.OK).To(BeTrue(), m.Message)
			Expect(m.Message).To(ContainSubstring("acme/lib (read only)"))
			Expect(m.Message).To(ContainSubstring("acme/other (admin)"))
			Expect(m.Message).To(ContainSubstring("administrator on 1"))
		})

		It("fails when the bot cannot push anywhere", func() {
			e := newEnv(func(f *fakegitea.Fake) {
				f.Collaborate("koinon-bot", "app", "read")
				f.Collaborate("koinon-bot", "lib", "read")
			})
			got := check(e)
			Expect(got["gitea.repositories"].OK).To(BeFalse())
			Expect(got["gitea.repositories"].Message).To(ContainSubstring("cannot push to any"))
			Expect(got["gitea.read-token"].Message).To(HavePrefix("skipped"))
		})

		It("fails when the bot sees nothing", func() {
			e := newEnv(func(f *fakegitea.Fake) {
				f.Collaborate("koinon-bot", "app", "")
				f.Collaborate("koinon-bot", "lib", "")
			})
			Expect(check(e)["gitea.repositories"].Message).To(ContainSubstring("cannot see any repository"))
		})

		It("fails, and skips the rest, when the tokens belong to different accounts", func() {
			e := newEnv(func(f *fakegitea.Fake) { f.AddScopedToken(readTok, "someone-else", "read") })
			got := check(e)
			Expect(got["gitea.tokens"].OK).To(BeFalse())
			Expect(got["gitea.tokens"].Message).To(ContainSubstring("different accounts"))
			Expect(got["gitea.repositories"].Message).To(HavePrefix("skipped"))
		})

		It("fails when the read token has a write scope", func() {
			e := newEnv(func(f *fakegitea.Fake) { f.AddScopedToken(readTok, "koinon-bot", "write") })
			m := check(e)["gitea.read-token"]
			Expect(m.OK).To(BeFalse())
			Expect(m.Message).To(ContainSubstring("has a write scope"))
		})

		It("explains a rejected token", func() {
			e := newEnv(func(f *fakegitea.Fake) { delete(f.Tokens, writeTok) })
			m := check(e)["gitea.tokens"]
			Expect(m.OK).To(BeFalse())
			Expect(m.Message).To(ContainSubstring("rejected the token"))
			Expect(m.Message).NotTo(ContainSubstring("not-a-real"))
		})

		It("explains a wrong server address", func() {
			e := newEnv(nil)
			e.conn = connection(e.fake.URL()+"/wrong", e.fake.CAPEM())
			m := e.host.Checks(ctx, e.conn, giteaType())[0]
			Expect(m.OK).To(BeFalse())
			Expect(m.Message).To(ContainSubstring("404"))
		})

		It("fails on a certificate no root signs when the connection has no CA bundle", func() {
			e := newEnv(nil)
			e.conn = connection(e.fake.URL(), "")
			m := e.host.Checks(ctx, e.conn, giteaType())[0]
			Expect(m.OK).To(BeFalse())
			Expect(m.Message).To(ContainSubstring("certificate"))
		})

		It("works for a server served under a sub-path", func() {
			e := newEnv(nil)
			mux := http.NewServeMux()
			mux.Handle("/gitea/", http.StripPrefix("/gitea", e.fake.Handler))
			srv := httptest.NewTLSServer(mux)
			DeferCleanup(srv.Close)
			e.fake.Server.Close() // not used; the sub-path server serves the same fake
			conn := connection(srv.URL+"/gitea", pemOf(srv))
			for _, r := range e.host.Checks(ctx, conn, giteaType()) {
				Expect(r.OK).To(BeTrue(), "%s: %s", r.Check, r.Message)
			}
			a, err := e.host.Open(ctx, conn, read)
			Expect(err).NotTo(HaveOccurred())
			_, err = a.Repository(ctx, app)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.host.BranchURL(conn, app, "x")).To(Equal(srv.URL + "/gitea/acme/app/src/branch/x"))
		})
	})

	Describe("the token age", func() {
		It("keeps the date of a token that did not change and dates a new one", func() {
			t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			t1 := t0.Add(100 * 24 * time.Hour)
			site := codehost.Site{BaseURL: "https://git.example.test"}
			c0 := gitea.NewConfig(site, []byte("koinon_test_not-a-real-r1"), []byte("koinon_test_not-a-real-w1"), nil, t0)
			c1 := gitea.NewConfig(site, []byte("koinon_test_not-a-real-r1"), []byte("koinon_test_not-a-real-w2"), &c0, t1)
			Expect(c1.ReadTokenSetAt).To(Equal(t0), "same read token")
			Expect(c1.WriteTokenSetAt).To(Equal(t1), "new write token")
			raw, _ := json.Marshal(c1)
			Expect(string(raw)).NotTo(ContainSubstring("not-a-real"), "only fingerprints are stored in the config")
			d, err := gitea.NewHost(gitea.Options{}).Describe(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.ReadTokenSetAt).To(Equal(t0))
			Expect(d.WriteTokenSetAt).To(Equal(t1))
			Expect(d.Site.BaseURL).To(Equal("https://git.example.test"))
		})
	})
})

// pemOf is the PEM certificate of a TLS test server.
func pemOf(srv *httptest.Server) string {
	return string(pemEncode(srv.Certificate().Raw))
}
