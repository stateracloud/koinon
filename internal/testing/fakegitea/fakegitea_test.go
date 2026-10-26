// SPDX-License-Identifier: Apache-2.0

package fakegitea_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

func TestFakeGitea(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Fake Gitea Suite")
}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r resp) JSON() map[string]any {
	var m map[string]any
	ExpectWithOffset(1, json.Unmarshal(r.Body, &m)).To(Succeed(), string(r.Body))
	return m
}

func (r resp) List() []map[string]any {
	var m []map[string]any
	ExpectWithOffset(1, json.Unmarshal(r.Body, &m)).To(Succeed(), string(r.Body))
	return m
}

var _ = Describe("fake Gitea", func() {
	var f *fakegitea.Fake

	// do sends a request authenticated with the default token as "token <t>".
	do := func(method, path string, body any) resp {
		return doAuth(f, method, path, body, "token "+fakegitea.DefaultToken)
	}
	const repo = "/api/v1/repos/acme/widgets"

	BeforeEach(func() {
		f = fakegitea.New()
		f.AddRepo("widgets")
		DeferCleanup(f.Close)
	})

	Describe("authentication", func() {
		It("accepts a token as the Basic password, with any user name", func() {
			basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakegitea.DefaultToken))
			r := doAuth(f, "GET", "/api/v1/user", nil, basic)
			Expect(r.Status).To(Equal(200))
			Expect(r.JSON()["login"]).To(Equal(fakegitea.DefaultLogin))
		})
		It("accepts the 'token <t>' header", func() {
			Expect(do("GET", "/api/v1/user", nil).Status).To(Equal(200))
		})
		It("rejects missing, wrong and other schemes", func() {
			for _, h := range []string{
				"", "token nope", "Bearer " + fakegitea.DefaultToken,
				"Basic " + base64.StdEncoding.EncodeToString([]byte("x:nope")),
				"Basic !!!not-base64", fakegitea.DefaultToken,
			} {
				r := doAuth(f, "GET", "/api/v1/user", nil, h)
				Expect(r.Status).To(Equal(401), h)
				Expect(r.JSON()).To(HaveKey("message"))
			}
		})
		It("accepts Bearer only when allowed", func() {
			f.AllowBearer = true
			Expect(doAuth(f, "GET", "/api/v1/user", nil, "Bearer "+fakegitea.DefaultToken).Status).To(Equal(200))
		})
		It("maps several tokens to their logins", func() {
			f.AddToken("other-fake-token", "reviewer")
			r := doAuth(f, "GET", "/api/v1/user", nil, "token other-fake-token")
			Expect(r.JSON()["login"]).To(Equal("reviewer"))
		})
		It("protects the repository endpoints too", func() {
			Expect(doAuth(f, "GET", repo, nil, "").Status).To(Equal(401))
			Expect(doAuth(f, "GET", "/api/v1/nope", nil, "").Status).To(Equal(401))
			Expect(do("GET", "/api/v1/nope", nil).Status).To(Equal(404))
		})
	})

	It("reports the version without authentication", func() {
		r := doAuth(f, "GET", "/api/v1/version", nil, "")
		Expect(r.Status).To(Equal(200))
		Expect(r.JSON()["version"]).To(Equal("28.2.0"))
	})

	Describe("repositories", func() {
		It("lists the repositories with a total count and pages", func() {
			f.AddRepo("gadgets")
			r := do("GET", "/api/v1/user/repos?limit=1&page=2", nil)
			Expect(r.Status).To(Equal(200))
			Expect(r.Header.Get("X-Total-Count")).To(Equal("2"))
			list := r.List()
			Expect(list).To(HaveLen(1))
			Expect(list[0]["full_name"]).To(Equal("acme/widgets"))
			Expect(do("GET", "/api/v1/user/repos?page=9", nil).List()).To(BeEmpty())
		})
		It("gets a repository, matching names case-insensitively", func() {
			m := do("GET", "/api/v1/repos/ACME/Widgets", nil).JSON()
			Expect(m).To(HaveKeyWithValue("full_name", "acme/widgets"))
			Expect(m).To(HaveKeyWithValue("default_branch", "main"))
			Expect(m["clone_url"]).To(HaveSuffix("/acme/widgets.git"))
		})
		It("answers 404 for unknown repositories and owners", func() {
			Expect(do("GET", "/api/v1/repos/acme/missing", nil).Status).To(Equal(404))
			Expect(do("GET", "/api/v1/repos/other/widgets", nil).Status).To(Equal(404))
		})
	})

	Describe("branches", func() {
		It("lists and gets branches with the head commit id", func() {
			list := do("GET", repo+"/branches", nil).List()
			Expect(list).To(HaveLen(1))
			Expect(list[0]["name"]).To(Equal("main"))
			sha := f.Snapshot("widgets").Branches["main"]
			one := do("GET", repo+"/branches/main", nil).JSON()
			Expect(one["commit"].(map[string]any)["id"]).To(Equal(sha))
			Expect(do("GET", repo+"/branches/nope", nil).Status).To(Equal(404))
		})
		It("honours the deprecated old_branch_name", func() {
			do("POST", repo+"/contents", map[string]any{"new_branch": "src", "files": []map[string]any{{"operation": "create", "path": "s", "content": "cw=="}}})
			Expect(do("POST", repo+"/branches", map[string]any{"new_branch_name": "cut", "old_branch_name": "src"}).Status).To(Equal(201))
			s := f.Snapshot("widgets")
			Expect(s.Branches["cut"]).To(Equal(s.Branches["src"]))
		})
		It("refuses refs that look like git options", func() {
			Expect(do("GET", repo+"/git/trees/--output=x", nil).Status).To(Equal(404))
			Expect(do("GET", repo+"/contents/README.md?ref=--output=x", nil).Status).To(Equal(404))
		})
		It("creates a branch from old_ref_name, and refuses duplicates", func() {
			r := do("POST", repo+"/branches", map[string]any{"new_branch_name": "feature/x", "old_ref_name": "main"})
			Expect(r.Status).To(Equal(201))
			s := f.Snapshot("widgets")
			Expect(s.Branches["feature/x"]).To(Equal(s.Branches["main"]))
			Expect(do("GET", repo+"/branches/feature/x", nil).Status).To(Equal(200))
			Expect(do("POST", repo+"/branches", map[string]any{"new_branch_name": "feature/x"}).Status).To(Equal(409))
			Expect(do("POST", repo+"/branches", map[string]any{"new_branch_name": "y", "old_ref_name": "nope"}).Status).To(Equal(404))
		})
	})

	Describe("contents", func() {
		It("reads a file with base64 content, and lists directories", func() {
			m := do("GET", repo+"/contents/README.md?ref=main", nil).JSON()
			Expect(m).To(HaveKeyWithValue("type", "file"))
			Expect(m).To(HaveKeyWithValue("encoding", "base64"))
			b, err := base64.StdEncoding.DecodeString(m["content"].(string))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(Equal("# widgets\n"))
			Expect(m["size"]).To(BeEquivalentTo(10))

			root := do("GET", repo+"/contents", nil).List()
			Expect(root).To(HaveLen(1))
			Expect(root[0]["name"]).To(Equal("README.md"))

			Expect(do("GET", repo+"/contents/missing.txt", nil).Status).To(Equal(404))
			Expect(do("GET", repo+"/contents/README.md?ref=nope", nil).Status).To(Equal(404))
		})
	})

	Describe("ChangeFiles", func() {
		change := func(body map[string]any) resp { return do("POST", repo+"/contents", body) }
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

		It("commits several files at once on a new branch from a base", func() {
			readme := do("GET", repo+"/contents/README.md", nil).JSON()
			r := change(map[string]any{
				"branch": "main", "new_branch": "agent/work", "message": "Add things",
				"files": []map[string]any{
					{"operation": "create", "path": "docs/a.txt", "content": b64("a\n")},
					{"operation": "update", "path": "README.md", "content": b64("# new\n"), "sha": readme["sha"]},
				},
			})
			Expect(r.Status).To(Equal(201), string(r.Body))
			m := r.JSON()
			Expect(m["files"]).To(HaveLen(2))
			sha := m["commit"].(map[string]any)["sha"].(string)

			s := f.Snapshot("widgets")
			Expect(s.Branches["agent/work"]).To(Equal(sha))
			Expect(s.Branches["main"]).NotTo(Equal(sha), "the base branch does not move")
			Expect(s.Calls["POST contents"]).To(Equal(1))

			a := do("GET", repo+"/contents/docs/a.txt?ref=agent/work", nil).JSON()
			Expect(a["path"]).To(Equal("docs/a.txt"))
			dir := do("GET", repo+"/contents/docs?ref=agent/work", nil).List()
			Expect(dir).To(HaveLen(1))
			Expect(dir[0]["path"]).To(Equal("docs/a.txt"))

			// The commit is readable through git/commits in Gitea's nested shape.
			c := do("GET", repo+"/git/commits/"+sha, nil).JSON()
			Expect(c["commit"].(map[string]any)["message"]).To(Equal("Add things\n"))
			Expect(c["parents"]).To(HaveLen(1))
			tree := c["commit"].(map[string]any)["tree"].(map[string]any)["sha"]
			Expect(do("GET", repo+"/git/trees/"+tree.(string), nil).JSON()["tree"]).To(HaveLen(2))
		})
		It("commits to an existing branch and moves it", func() {
			r := change(map[string]any{"branch": "main", "files": []map[string]any{{"operation": "create", "path": "x.txt", "content": b64("x")}}})
			Expect(r.Status).To(Equal(201))
			Expect(f.Snapshot("widgets").Branches["main"]).To(Equal(r.JSON()["commit"].(map[string]any)["sha"]))
		})
		It("deletes and renames files", func() {
			readme := do("GET", repo+"/contents/README.md", nil).JSON()
			r := change(map[string]any{"new_branch": "mv", "files": []map[string]any{
				{"operation": "rename", "from_path": "README.md", "path": "docs/README.md"},
			}})
			Expect(r.Status).To(Equal(201), string(r.Body))
			Expect(do("GET", repo+"/contents/docs/README.md?ref=mv", nil).JSON()["sha"]).To(Equal(readme["sha"]))
			Expect(do("GET", repo+"/contents/README.md?ref=mv", nil).Status).To(Equal(404))

			r = change(map[string]any{"branch": "mv", "files": []map[string]any{
				{"operation": "delete", "path": "docs/README.md", "sha": readme["sha"]},
			}})
			Expect(r.Status).To(Equal(201), string(r.Body))
			Expect(do("GET", repo+"/contents?ref=mv", nil).List()).To(BeEmpty())
		})
		It("updates and deletes without a sha, and treats upload as create-or-update", func() {
			one := func(op, p, c string) resp {
				return change(map[string]any{"files": []map[string]any{{"operation": op, "path": p, "content": b64(c)}}})
			}
			Expect(one("update", "README.md", "v2").Status).To(Equal(201))
			Expect(one("upload", "README.md", "v3").Status).To(Equal(201), "upload over a file")
			Expect(one("upload", "new.txt", "n").Status).To(Equal(201), "upload of a new file")
			Expect(one("create", "my.git/x", "y").Status).To(Equal(201), "my.git is a legal directory")
			Expect(one("delete", "new.txt", "").Status).To(Equal(201))
			Expect(do("GET", repo+"/contents/new.txt", nil).Status).To(Equal(404))
		})
		It("refuses what Gitea refuses", func() {
			readme := do("GET", repo+"/contents/README.md", nil).JSON()
			file := func(op, path, sha string) []map[string]any {
				return []map[string]any{{"operation": op, "path": path, "content": b64("z"), "sha": sha}}
			}
			Expect(change(map[string]any{"files": file("create", "README.md", "")}).Status).To(Equal(422), "create over a file")
			Expect(change(map[string]any{"files": file("update", "README.md", "deadbeef")}).Status).To(Equal(422), "stale sha")
			Expect(change(map[string]any{"files": file("update", "nope", readme["sha"].(string))}).Status).To(Equal(404), "update missing")
			Expect(change(map[string]any{"files": file("delete", "nope", "x")}).Status).To(Equal(404), "delete missing")
			Expect(change(map[string]any{"branch": "nope", "files": file("create", "a", "")}).Status).To(Equal(404), "missing branch")
			Expect(change(map[string]any{"new_branch": "main", "files": file("create", "a", "")}).Status).To(Equal(422), "new_branch exists")
			Expect(change(map[string]any{"files": file("", "a", "")}).Status).To(Equal(422), "operation required")
			Expect(change(map[string]any{"files": file("create", "README.md/x", "")}).Status).To(Equal(422), "parent is a file")
			Expect(change(map[string]any{"files": file("create", ".GIT/x", "")}).Status).To(Equal(422), ".GIT component")
			Expect(change(map[string]any{"files": file("create", "../x", "")}).Status).To(Equal(422), "path escape")
			Expect(change(map[string]any{"files": file("create", ".git/config", "")}).Status).To(Equal(422), ".git")
			Expect(change(map[string]any{"files": []map[string]any{}}).Status).To(Equal(422), "no files")
			Expect(change(map[string]any{"files": []map[string]any{{"operation": "create", "path": "a", "content": "!!"}}}).Status).To(Equal(422), "bad base64")
			Expect(f.Snapshot("widgets").Calls).NotTo(HaveKey("POST contents"))
		})
		It("keeps the executable bit of an updated file and makes new files regular", func() {
			dir := f.RepoDir("widgets")
			// Push an executable through git itself, then update it through the API.
			work := GinkgoT().TempDir()
			git(work, "init", "-q", "-b", "main")
			Expect(os.WriteFile(filepath.Join(work, "run.sh"), []byte("#!/bin/sh\n"), 0o755)).To(Succeed())
			git(work, "add", "run.sh")
			git(work, "commit", "-q", "-m", "exec")
			git(work, "push", "-q", "--force", dir, "main:exec")
			sha := do("GET", repo+"/contents/run.sh?ref=exec", nil).JSON()["sha"]
			r := change(map[string]any{"branch": "exec", "files": []map[string]any{
				{"operation": "update", "path": "run.sh", "content": b64("#!/bin/sh\necho\n"), "sha": sha},
				{"operation": "create", "path": "new.sh", "content": b64("x")},
			}})
			Expect(r.Status).To(Equal(201), string(r.Body))
			ls := git(dir, "ls-tree", "exec")
			Expect(ls).To(ContainSubstring("100755 blob"))
			Expect(ls).To(MatchRegexp(`100644 blob \w+\s+new.sh`))
		})
	})

	Describe("git trees", func() {
		It("lists recursively with paging and 404s for unknown objects", func() {
			b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
			do("POST", repo+"/contents", map[string]any{"files": []map[string]any{
				{"operation": "create", "path": "a/b/c.txt", "content": b64("c")}, {"operation": "create", "path": "a/d.txt", "content": b64("d")},
			}})
			flat := do("GET", repo+"/git/trees/main", nil).JSON()
			Expect(flat["tree"]).To(HaveLen(2)) // README.md and the directory a
			rec := do("GET", repo+"/git/trees/main?recursive=1", nil).JSON()
			Expect(rec["total_count"]).To(BeEquivalentTo(5)) // README.md, a, a/b, a/b/c.txt, a/d.txt
			paged := do("GET", repo+"/git/trees/main?recursive=true&per_page=2&page=3", nil).JSON()
			Expect(paged["tree"]).To(HaveLen(1))
			Expect(paged["truncated"]).To(BeFalse())
			Expect(do("GET", repo+"/git/trees/main?recursive=1&per_page=2", nil).JSON()["truncated"]).To(BeTrue())
			Expect(do("GET", repo+"/git/trees/0000000000000000000000000000000000000000", nil).Status).To(Equal(404))
			Expect(do("GET", repo+"/git/commits/nope", nil).Status).To(Equal(404))
		})
	})

	Describe("pull requests", func() {
		var featureSHA string
		BeforeEach(func() {
			r := do("POST", repo+"/contents", map[string]any{
				"new_branch": "feature", "message": "work",
				"files": []map[string]any{{"operation": "create", "path": "f.txt", "content": base64.StdEncoding.EncodeToString([]byte("f"))}},
			})
			Expect(r.Status).To(Equal(201))
			featureSHA = f.Snapshot("widgets").Branches["feature"]
		})
		create := func(head string) resp {
			return do("POST", repo+"/pulls", map[string]any{"title": "Add f", "body": "body", "head": head, "base": "main"})
		}

		It("creates, finds and lists pull requests", func() {
			r := create("feature")
			Expect(r.Status).To(Equal(201), string(r.Body))
			m := r.JSON()
			Expect(m).To(HaveKeyWithValue("number", BeEquivalentTo(1)))
			Expect(m).To(HaveKeyWithValue("state", "open"))
			Expect(m["head"].(map[string]any)["sha"]).To(Equal(featureSHA))
			Expect(m["head"].(map[string]any)["ref"]).To(Equal("feature"))
			Expect(m["base"].(map[string]any)["ref"]).To(Equal("main"))
			Expect(m["html_url"]).To(HaveSuffix("/acme/widgets/pulls/1"))
			Expect(m["user"].(map[string]any)["login"]).To(Equal(fakegitea.DefaultLogin))

			Expect(do("GET", repo+"/pulls/1", nil).JSON()["title"]).To(Equal("Add f"))
			Expect(do("GET", repo+"/pulls/9", nil).Status).To(Equal(404))
			Expect(do("GET", repo+"/pulls/main/feature", nil).JSON()["number"]).To(BeEquivalentTo(1))
			Expect(do("GET", repo+"/pulls/main/acme:feature", nil).JSON()["number"]).To(BeEquivalentTo(1))
			Expect(do("GET", repo+"/pulls/main/other", nil).Status).To(Equal(404))

			list := do("GET", repo+"/pulls?state=open", nil)
			Expect(list.List()).To(HaveLen(1))
			Expect(list.Header.Get("X-Total-Count")).To(Equal("1"))
			Expect(do("GET", repo+"/pulls?state=closed", nil).List()).To(BeEmpty())
			Expect(do("GET", repo+"/pulls?state=all", nil).List()).To(HaveLen(1))
			Expect(f.Snapshot("widgets").Calls["POST pulls"]).To(Equal(1))
		})
		It("accepts the owner:branch head GitHub clients write", func() {
			Expect(create("acme:feature").Status).To(Equal(201))
			Expect(f.Snapshot("widgets").Pulls[0].Head).To(Equal("feature"))
		})
		It("refuses a duplicate with 409, a missing branch with 404 and a bad body with 422", func() {
			Expect(create("feature").Status).To(Equal(201))
			Expect(create("feature").Status).To(Equal(409))
			Expect(create("nope").Status).To(Equal(404))
			Expect(do("POST", repo+"/pulls", map[string]any{"title": "t", "head": "feature", "base": "nope"}).Status).To(Equal(404))
			Expect(do("POST", repo+"/pulls", map[string]any{"head": "feature", "base": "main"}).Status).To(Equal(422))
			Expect(do("POST", repo+"/pulls", map[string]any{"title": "t", "base": "main"}).Status).To(Equal(422))
			Expect(do("POST", repo+"/pulls", map[string]any{"title": "t", "head": "main", "base": "main"}).Status).To(Equal(422))
			Expect(f.Snapshot("widgets").Pulls).To(HaveLen(1))
		})
		It("requires the base like Gitea", func() {
			r := do("POST", repo+"/pulls", map[string]any{"title": "t", "head": "feature"})
			Expect(r.Status).To(Equal(422))
		})
	})

	Describe("issue comments", func() {
		It("creates and lists comments on an issue and on a pull request", func() {
			n := f.AddIssue("widgets", "bug")
			Expect(n).To(BeEquivalentTo(1))
			path := repo + "/issues/1/comments"
			Expect(do("GET", path, nil).List()).To(BeEmpty())

			r := do("POST", path, map[string]any{"body": "<!-- koinon:marker -->\nhello"})
			Expect(r.Status).To(Equal(201))
			Expect(r.JSON()["body"]).To(ContainSubstring("hello"))
			Expect(r.JSON()["user"].(map[string]any)["login"]).To(Equal(fakegitea.DefaultLogin))
			do("POST", path, map[string]any{"body": "second"})

			list := do("GET", path, nil).List()
			Expect(list).To(HaveLen(2))
			Expect(list[0]["id"]).To(BeNumerically("<", list[1]["id"]))
			Expect(do("GET", path+"?since=2999-01-01T00:00:00Z", nil).List()).To(BeEmpty())
			Expect(do("GET", path+"?before=2999-01-01T00:00:00Z", nil).List()).To(HaveLen(2))
			Expect(do("GET", path+"?before=2000-01-01T00:00:00Z", nil).List()).To(BeEmpty())

			// A pull request shares the issue numbering.
			do("POST", repo+"/contents", map[string]any{"new_branch": "p", "files": []map[string]any{{"operation": "create", "path": "p", "content": "cA=="}}})
			pr := do("POST", repo+"/pulls", map[string]any{"title": "t", "head": "p", "base": "main"}).JSON()
			Expect(pr["number"]).To(BeEquivalentTo(2))
			Expect(do("POST", repo+"/issues/2/comments", map[string]any{"body": "on the PR"}).Status).To(Equal(201))
			Expect(f.Snapshot("widgets").Comments).To(HaveLen(3))
			Expect(f.Snapshot("widgets").Calls["POST comments"]).To(Equal(3))
		})
		It("refuses unknown issues and empty bodies", func() {
			Expect(do("GET", repo+"/issues/7/comments", nil).Status).To(Equal(404))
			Expect(do("POST", repo+"/issues/7/comments", map[string]any{"body": "x"}).Status).To(Equal(404))
			f.AddIssue("widgets", "bug")
			Expect(do("POST", repo+"/issues/1/comments", map[string]any{"body": ""}).Status).To(Equal(422))
		})
	})

	Describe("git smart HTTP", func() {
		BeforeEach(func() {
			if _, err := exec.LookPath("git"); err != nil {
				Skip("git is not installed")
			}
		})
		cloneURL := func(token string) string {
			return strings.Replace(f.Server.URL, "http://", "http://x-access-token:"+token+"@", 1) + "/acme/widgets.git"
		}

		It("rejects clones without a valid token", func() {
			dst := filepath.Join(GinkgoT().TempDir(), "c")
			out, err := gitErr(GinkgoT().TempDir(), "clone", cloneURL("wrong"), dst)
			Expect(err).To(HaveOccurred(), out)
			out, err = gitErr(GinkgoT().TempDir(), "clone", f.Server.URL+"/acme/widgets.git", dst)
			Expect(err).To(HaveOccurred(), out)
			out, err = gitErr(GinkgoT().TempDir(), "clone", strings.Replace(f.Server.URL, "http://", "http://x:"+fakegitea.DefaultToken+"@", 1)+"/acme/missing.git", dst)
			Expect(err).To(HaveOccurred(), out)
		})

		It("clones, pushes a branch, and the REST API and a second clone see it", func() {
			tmp := GinkgoT().TempDir()
			one := filepath.Join(tmp, "one")
			git(tmp, "clone", "-q", cloneURL(fakegitea.DefaultToken), one)
			Expect(os.ReadFile(filepath.Join(one, "README.md"))).To(Equal([]byte("# widgets\n")))

			git(one, "checkout", "-q", "-b", "feature")
			Expect(os.WriteFile(filepath.Join(one, "hello.txt"), []byte("hello\n"), 0o644)).To(Succeed())
			git(one, "add", ".")
			git(one, "commit", "-q", "-m", "hello")
			git(one, "push", "-q", "origin", "feature")
			head := strings.TrimSpace(git(one, "rev-parse", "HEAD"))

			Expect(f.Snapshot("widgets").Branches["feature"]).To(Equal(head))
			Expect(doAuth(f, "GET", repo+"/branches/feature", nil, "token "+fakegitea.DefaultToken).JSON()["commit"].(map[string]any)["id"]).To(Equal(head))
			c := doAuth(f, "GET", repo+"/contents/hello.txt?ref=feature", nil, "token "+fakegitea.DefaultToken).JSON()
			Expect(c["path"]).To(Equal("hello.txt"))

			// A pull request opened through the API for the pushed branch.
			pr := doAuth(f, "POST", repo+"/pulls", map[string]any{"title": "hello", "head": "feature", "base": "main"}, "token "+fakegitea.DefaultToken)
			Expect(pr.Status).To(Equal(201), string(pr.Body))

			// A commit made through ChangeFiles is fetched by git, and a fast-forward push works.
			ch := doAuth(f, "POST", repo+"/contents", map[string]any{
				"branch": "feature", "message": "via api",
				"files": []map[string]any{{"operation": "create", "path": "api.txt", "content": base64.StdEncoding.EncodeToString([]byte("api\n"))}},
			},
				"token "+fakegitea.DefaultToken)
			Expect(ch.Status).To(Equal(201), string(ch.Body))
			git(one, "pull", "-q", "--ff-only", "origin", "feature")
			Expect(os.ReadFile(filepath.Join(one, "api.txt"))).To(Equal([]byte("api\n")))
			Expect(os.WriteFile(filepath.Join(one, "more.txt"), []byte("more\n"), 0o644)).To(Succeed())
			git(one, "add", ".")
			git(one, "commit", "-q", "-m", "more")
			git(one, "push", "-q", "origin", "feature")

			two := filepath.Join(tmp, "two")
			git(tmp, "clone", "-q", "--branch", "feature", cloneURL(fakegitea.DefaultToken), two)
			Expect(git(two, "log", "--format=%s")).To(Equal("more\nvia api\nhello\nInitial commit\n"))

			// Deleting the branch over git is reflected too.
			git(one, "push", "-q", "origin", "--delete", "feature")
			Expect(f.Snapshot("widgets").Branches).NotTo(HaveKey("feature"))
		})

		It("accepts a push larger than http.postBuffer (chunked request body)", func() {
			tmp := GinkgoT().TempDir()
			one := filepath.Join(tmp, "one")
			git(tmp, "clone", "-q", cloneURL(fakegitea.DefaultToken), one)
			blob := make([]byte, 4<<20)
			rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // incompressible filler, not a secret
			for i := range blob {
				blob[i] = byte(rng.UintN(256))
			}
			Expect(os.WriteFile(filepath.Join(one, "big.bin"), blob, 0o644)).To(Succeed())
			git(one, "add", ".")
			git(one, "commit", "-q", "-m", "big")
			git(one, "push", "-q", "origin", "HEAD:refs/heads/big")
			Expect(f.Snapshot("widgets").Branches["big"]).To(Equal(strings.TrimSpace(git(one, "rev-parse", "HEAD"))))
		})

		It("serves the owner/repo path case-insensitively", func() {
			tmp := GinkgoT().TempDir()
			url := strings.Replace(f.Server.URL, "http://", "http://x:"+fakegitea.DefaultToken+"@", 1) + "/ACME/Widgets.git"
			git(tmp, "clone", "-q", url, filepath.Join(tmp, "c"))
		})
	})
})

func doAuth(f *fakegitea.Fake, method, path string, body any, authorization string) resp {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.Server.URL+path, rd)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := http.DefaultClient.Do(req)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

// gitErr runs git hermetically in dir and returns its combined output.
func gitErr(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.invalid",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func git(dir string, args ...string) string {
	out, err := gitErr(dir, args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return out
}
