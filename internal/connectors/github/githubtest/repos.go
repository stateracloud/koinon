// SPDX-License-Identifier: Apache-2.0

package githubtest

import (
	"crypto/sha1" //nolint:gosec // git object IDs, not security
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Repo is the fake's state of one repository: a git object store with branches, pull
// requests and issue comments, enough for the Git Data, Pulls and Issues endpoints Koinon
// calls. Every repository of the installation starts with a main branch holding README.md.
type Repo struct {
	Name          string
	DefaultBranch string
	Refs          map[string]string // branch -> commit SHA
	Commits       map[string]Commit
	Trees         map[string]map[string]string // tree SHA -> path -> blob SHA
	Blobs         map[string][]byte
	Pulls         []Pull
	Comments      map[int64][]Comment
	// FailContents makes GET contents of a path answer with the status instead.
	FailContents map[string]int
	// Calls counts write calls by endpoint, e.g. "POST pulls".
	Calls map[string]int
}

// Commit is a commit object.
type Commit struct {
	SHA, Message, Tree string
	Parents            []string
}

// Pull is a pull request.
type Pull struct {
	Number           int64
	Title, Body      string
	Head, Base       string
	HeadSHA, HTMLURL string
}

// Comment is an issue comment.
type Comment struct {
	ID      int64
	Body    string
	HTMLURL string
}

func objectID(kind string, parts ...string) string {
	h := sha1.New() //nolint:gosec // git object IDs, not security
	h.Write([]byte(kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (f *Fake) repo(name string) *Repo {
	r, ok := f.repos[name]
	if ok {
		return r
	}
	r = &Repo{
		Name: name, DefaultBranch: "main", Refs: map[string]string{}, Commits: map[string]Commit{},
		Trees: map[string]map[string]string{}, Blobs: map[string][]byte{}, Comments: map[int64][]Comment{},
		FailContents: map[string]int{}, Calls: map[string]int{},
	}
	readme := []byte("# " + name + "\n")
	blob := objectID("blob", string(readme))
	r.Blobs[blob] = readme
	tree := objectID("tree", "README.md", blob)
	r.Trees[tree] = map[string]string{"README.md": blob}
	c := Commit{SHA: objectID("commit", "initial", tree), Message: "initial", Tree: tree}
	r.Commits[c.SHA] = c
	r.Refs["main"] = c.SHA
	f.repos[name] = r
	return r
}

// Files returns the files of a branch's head commit, or nil if the branch does not exist.
func (f *Fake) Files(repo, branch string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	sha, ok := r.Refs[branch]
	if !ok {
		return nil
	}
	out := map[string][]byte{}
	for p, b := range r.Trees[r.Commits[sha].Tree] {
		out[p] = slices.Clone(r.Blobs[b])
	}
	return out
}

// Snapshot returns a copy of a repository's state.
func (f *Fake) Snapshot(repo string) Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	cp := *r
	cp.Refs, cp.Commits, cp.Calls = maps.Clone(r.Refs), maps.Clone(r.Commits), maps.Clone(r.Calls)
	cp.Pulls = slices.Clone(r.Pulls)
	cp.Comments = map[int64][]Comment{}
	for k, v := range r.Comments {
		cp.Comments[k] = slices.Clone(v)
	}
	return cp
}

// SetBranch points a branch at a new commit with the given files on top of the default
// branch, as a human pushing to it would.
func (f *Fake) SetBranch(repo, branch, message string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	base := r.Commits[r.Refs[r.DefaultBranch]]
	tree := maps.Clone(r.Trees[base.Tree])
	for p, content := range files {
		b := objectID("blob", content)
		r.Blobs[b] = []byte(content)
		tree[p] = b
	}
	tsha := treeID(tree)
	r.Trees[tsha] = tree
	c := Commit{SHA: objectID("commit", message, tsha, base.SHA), Message: message, Tree: tsha, Parents: []string{base.SHA}}
	r.Commits[c.SHA] = c
	r.Refs[branch] = c.SHA
	return c.SHA
}

// FailContents makes the contents API answer status for path in repo.
func (f *Fake) FailContents(repo, path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).FailContents[path] = status
}

func treeID(tree map[string]string) string {
	var parts []string
	for _, p := range slices.Sorted(maps.Keys(tree)) {
		parts = append(parts, p, tree[p])
	}
	return objectID("tree", parts...)
}

func (f *Fake) routeRepos(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/{owner}/{repo}", f.withRepo("metadata", "read", f.getRepo))
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", f.withRepo("contents", "read", f.getContents))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", f.withRepo("contents", "read", f.getRef))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs", f.withRepo("contents", "write", f.createRef))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}", f.withRepo("contents", "read", f.getCommit))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/commits", f.withRepo("contents", "write", f.createCommit))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/blobs", f.withRepo("contents", "write", f.createBlob))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/trees", f.withRepo("contents", "write", f.createTree))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", f.withRepo("pull_requests", "read", f.listPulls))
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", f.withRepo("pull_requests", "write", f.createPull))
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments", f.withRepo("issues", "read", f.listComments))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", f.withRepo("issues", "write", f.createComment))
}

// withRepo checks the installation token as GitHub does: the token must be one the fake
// minted, cover the repository, and carry the permission at the level (write implies read;
// every token can read metadata). The handler runs with f.mu held.
func (f *Fake) withRepo(perm, level string, h func(http.ResponseWriter, *http.Request, *Repo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || r.Header.Get("X-GitHub-Api-Version") == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		info, ok := f.tokens[tok]
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		name, ok := f.canonical(r.PathValue("repo"))
		if !strings.EqualFold(r.PathValue("owner"), f.Account) || !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		if !slices.Contains(info.repos, name) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		have := info.perms[perm]
		if perm == "metadata" {
			have = "read"
		}
		if have == "" || (level == "write" && have != "write") {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
			return
		}
		repo := f.repo(name)
		if r.Method != http.MethodGet {
			key := r.Method + " " + strings.TrimPrefix(r.Pattern, "POST /repos/{owner}/{repo}/")
			repo.Calls[key]++
		}
		h(w, r, repo)
	}
}

func (f *Fake) getRepo(w http.ResponseWriter, _ *http.Request, r *Repo) {
	writeJSON(w, http.StatusOK, map[string]any{"full_name": f.Account + "/" + r.Name, "default_branch": r.DefaultBranch})
}

// maxContentsSize is the largest file the contents API returns with its content (1 MB).
const maxContentsSize = 1 << 20

// getContents serves a path at ?ref= (a branch or a commit SHA; default the default
// branch) as GitHub does: base64 content up to 1 MB, encoding "none" above, an array for a
// directory.
func (f *Fake) getContents(w http.ResponseWriter, req *http.Request, r *Repo) {
	p := req.PathValue("path")
	if status, ok := r.FailContents[p]; ok {
		writeJSON(w, status, map[string]string{"message": http.StatusText(status)})
		return
	}
	ref := req.URL.Query().Get("ref")
	if ref == "" {
		ref = r.DefaultBranch
	}
	sha, ok := r.Refs[ref]
	if !ok {
		sha = ref
	}
	c, ok := r.Commits[sha]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No commit found for the ref " + ref})
		return
	}
	tree := r.Trees[c.Tree]
	blob, ok := tree[p]
	if !ok {
		var entries []map[string]any
		for _, name := range slices.Sorted(maps.Keys(tree)) {
			if strings.HasPrefix(name, p+"/") {
				entries = append(entries, map[string]any{"type": "file", "path": name})
			}
		}
		if entries == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, entries)
		return
	}
	content := r.Blobs[blob]
	out := map[string]any{"type": "file", "path": p, "sha": blob, "size": len(content), "encoding": "base64", "content": ""}
	if len(content) > maxContentsSize {
		out["encoding"] = "none"
	} else {
		// GitHub wraps the base64 at 60 columns.
		enc := base64.StdEncoding.EncodeToString(content)
		var b strings.Builder
		for len(enc) > 60 {
			b.WriteString(enc[:60] + "\n")
			enc = enc[60:]
		}
		b.WriteString(enc)
		out["content"] = b.String()
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) getRef(w http.ResponseWriter, req *http.Request, r *Repo) {
	branch := req.PathValue("branch")
	sha, ok := r.Refs[branch]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
}

func (f *Fake) createRef(w http.ResponseWriter, req *http.Request, r *Repo) {
	var in struct{ Ref, SHA string }
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	branch, ok := strings.CutPrefix(in.Ref, "refs/heads/")
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference name must start with refs/"})
		return
	}
	if _, exists := r.Refs[branch]; exists {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
		return
	}
	if _, ok := r.Commits[in.SHA]; !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Object does not exist"})
		return
	}
	r.Refs[branch] = in.SHA
	writeJSON(w, http.StatusCreated, map[string]any{"ref": in.Ref, "object": map[string]string{"type": "commit", "sha": in.SHA}})
}

func commitJSON(c Commit) map[string]any {
	parents := make([]map[string]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, map[string]string{"sha": p})
	}
	return map[string]any{"sha": c.SHA, "message": c.Message, "tree": map[string]string{"sha": c.Tree}, "parents": parents}
}

func (f *Fake) getCommit(w http.ResponseWriter, req *http.Request, r *Repo) {
	c, ok := r.Commits[req.PathValue("sha")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, commitJSON(c))
}

func (f *Fake) createCommit(w http.ResponseWriter, req *http.Request, r *Repo) {
	var in struct {
		Message, Tree string
		Parents       []string
	}
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	if _, ok := r.Trees[in.Tree]; !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Tree SHA does not exist"})
		return
	}
	for _, p := range in.Parents {
		if _, ok := r.Commits[p]; !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Parent SHA does not exist"})
			return
		}
	}
	c := Commit{SHA: objectID("commit", append([]string{in.Message, in.Tree}, in.Parents...)...), Message: in.Message, Tree: in.Tree, Parents: in.Parents}
	r.Commits[c.SHA] = c
	writeJSON(w, http.StatusCreated, commitJSON(c))
}

func (f *Fake) createBlob(w http.ResponseWriter, req *http.Request, r *Repo) {
	var in struct{ Content, Encoding string }
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	content := []byte(in.Content)
	if in.Encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid base64"})
			return
		}
		content = b
	}
	sha := objectID("blob", string(content))
	r.Blobs[sha] = content
	writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})
}

func (f *Fake) createTree(w http.ResponseWriter, req *http.Request, r *Repo) {
	var in struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path, Mode, Type string
			SHA              *string
		}
	}
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	tree := map[string]string{}
	if in.BaseTree != "" {
		base, ok := r.Trees[in.BaseTree]
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "base_tree is not a valid tree"})
			return
		}
		tree = maps.Clone(base)
	}
	for _, e := range in.Tree {
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "unsupported tree entry"})
			return
		}
		if e.SHA == nil {
			delete(tree, e.Path)
			continue
		}
		if _, ok := r.Blobs[*e.SHA]; !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "tree.sha is not a valid blob"})
			return
		}
		tree[e.Path] = *e.SHA
	}
	sha := treeID(tree)
	r.Trees[sha] = tree
	writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})
}

func (f *Fake) pullJSON(p Pull) map[string]any {
	return map[string]any{
		"number": p.Number, "html_url": p.HTMLURL, "state": "open", "title": p.Title, "body": p.Body,
		"head": map[string]string{"ref": p.Head, "sha": p.HeadSHA},
		"base": map[string]string{"ref": p.Base},
	}
}

func (f *Fake) listPulls(w http.ResponseWriter, req *http.Request, r *Repo) {
	head := req.URL.Query().Get("head")
	out := []map[string]any{}
	for _, p := range r.Pulls {
		if head == "" || head == f.Account+":"+p.Head {
			out = append(out, f.pullJSON(p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) createPull(w http.ResponseWriter, req *http.Request, r *Repo) {
	var in struct{ Title, Body, Head, Base string }
	if json.NewDecoder(req.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	sha, ok := r.Refs[in.Head]
	if _, base := r.Refs[in.Base]; !ok || !base {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	for _, p := range r.Pulls {
		if p.Head == in.Head && p.Base == in.Base {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "A pull request already exists for " + f.Account + ":" + in.Head + "."})
			return
		}
	}
	n := int64(len(r.Pulls) + 1)
	p := Pull{
		Number: n, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, HeadSHA: sha,
		HTMLURL: fmt.Sprintf("https://github.test/%s/%s/pull/%d", f.Account, r.Name, n),
	}
	r.Pulls = append(r.Pulls, p)
	writeJSON(w, http.StatusCreated, f.pullJSON(p))
}

func (f *Fake) listComments(w http.ResponseWriter, req *http.Request, r *Repo) {
	n, _ := strconv.ParseInt(req.PathValue("number"), 10, 64)
	page, _ := strconv.Atoi(req.URL.Query().Get("page"))
	per, _ := strconv.Atoi(req.URL.Query().Get("per_page"))
	page, per = max(page, 1), min(max(per, 1), 100)
	all := r.Comments[n]
	out := []map[string]any{}
	for i := (page - 1) * per; i < len(all) && i < page*per; i++ {
		c := all[i]
		out = append(out, map[string]any{"id": c.ID, "body": c.Body, "html_url": c.HTMLURL})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) createComment(w http.ResponseWriter, req *http.Request, r *Repo) {
	n, err := strconv.ParseInt(req.PathValue("number"), 10, 64)
	var in struct{ Body string }
	if err != nil || json.NewDecoder(req.Body).Decode(&in) != nil || in.Body == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	id := int64(1000 + len(r.Comments[n]))
	c := Comment{ID: id, Body: in.Body, HTMLURL: fmt.Sprintf("https://github.test/%s/%s/issues/%d#issuecomment-%d", f.Account, r.Name, n, id)}
	r.Comments[n] = append(r.Comments[n], c)
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "body": c.Body, "html_url": c.HTMLURL})
}
