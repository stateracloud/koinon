// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
)

func (f *Fake) gitUser(s sig) map[string]any {
	return map[string]any{"name": s.Name, "email": s.Email, "date": ts(s.When)}
}

// getGitCommit answers GET git/commits/{sha} in Gitea's shape: the Commit of
// repoGetSingleCommit, with the git commit nested under "commit" (GitHub's differs).
func (f *Fake) getGitCommit(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	sha, ok := rp.resolve(r.PathValue("sha"))
	if !ok {
		return errf(http.StatusNotFound, "object does not exist [id: %s, rel_path: ]", r.PathValue("sha"))
	}
	c, _ := rp.commitInfo(sha)
	writeJSON(w, http.StatusOK, f.fullCommitJSON(baseURL(r), rp, c))
	return nil
}

func (f *Fake) fullCommitJSON(base string, rp *repo, c gitCommit) map[string]any {
	repoURL := base + "/api/v1/repos/" + f.Owner + "/" + rp.name
	parents := []map[string]any{}
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p, "url": repoURL + "/commits/" + p, "created": ts(c.Committer.When)})
	}
	return map[string]any{
		"url": repoURL + "/git/commits/" + c.SHA, "sha": c.SHA, "created": ts(c.Committer.When),
		"html_url": base + "/" + f.Owner + "/" + rp.name + "/commit/" + c.SHA,
		"commit": map[string]any{
			"url": repoURL + "/git/commits/" + c.SHA, "author": f.gitUser(c.Author), "committer": f.gitUser(c.Committer),
			"message":      c.Message,
			"tree":         map[string]any{"url": repoURL + "/git/trees/" + c.Tree, "sha": c.Tree, "created": ts(c.Committer.When)},
			"verification": map[string]any{"verified": false, "reason": "gpg.error.not_signed_commit", "signature": "", "payload": ""},
		},
		"author": nil, "committer": nil, "parents": parents, "files": nil, "stats": nil,
	}
}

// getTree answers GET git/trees/{sha}, paged as Gitea does (per_page, page, truncated).
func (f *Fake) getTree(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	ref := r.PathValue("sha")
	// A branch, a tag, a commit, or a tree SHA. Anything else is a 404; never pass a raw value
	// (which could start with "-") to git.
	var treeish string
	if sha, ok := rp.resolve(ref); ok {
		treeish = sha
	} else if isHex(ref) && len(ref) >= 7 {
		treeish = ref
	} else {
		return errf(http.StatusNotFound, "object does not exist [id: %s, rel_path: ]", ref)
	}
	q := r.URL.Query()
	recursive := q.Get("recursive") == "1" || q.Get("recursive") == "true"
	es, err := rp.lsTree(treeish, recursive)
	if err != nil {
		return errf(http.StatusNotFound, "object does not exist [id: %s, rel_path: ]", ref)
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	pageN, _ := strconv.Atoi(q.Get("page"))
	if perPage < 1 {
		perPage = 1000
	}
	perPage = min(perPage, 1000)
	pageN = max(pageN, 1)
	start := min((pageN-1)*perPage, len(es))
	end := min(start+perPage, len(es))
	repoURL := baseURL(r) + "/api/v1/repos/" + f.Owner + "/" + rp.name
	tree := []map[string]any{}
	for _, e := range es[start:end] {
		tree = append(tree, map[string]any{
			"path": e.Path, "mode": e.Mode, "type": e.Type, "size": e.Size, "sha": e.SHA,
			"url": repoURL + "/git/" + e.Type + "s/" + e.SHA,
		})
	}
	sha := ref
	if out, err := rp.git(nil, nil, "rev-parse", treeish+"^{tree}"); err == nil {
		sha = strings.TrimSpace(out)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha": sha, "url": repoURL + "/git/trees/" + sha, "tree": tree, "truncated": end < len(es),
		"page": pageN, "total_count": len(es),
	})
	return nil
}

func (f *Fake) contentEntry(base string, rp *repo, ref, commit string, e entry, withContent bool) map[string]any {
	repoURL := base + "/api/v1/repos/" + f.Owner + "/" + rp.name
	html := base + "/" + f.Owner + "/" + rp.name
	typ := map[string]string{"blob": "file", "tree": "dir", "commit": "submodule"}[e.Type]
	if e.Type == "blob" && e.Mode == "120000" {
		typ = "symlink"
	}
	last, _ := rp.git(nil, nil, "log", "-1", "--format=%H", commit, "--", e.Path)
	m := map[string]any{
		"name": path.Base(e.Path), "path": e.Path, "sha": e.SHA, "last_commit_sha": strings.TrimSpace(last),
		"type": typ, "size": e.Size, "encoding": nil, "content": nil, "target": nil,
		"url":      repoURL + "/contents/" + e.Path + "?ref=" + ref,
		"html_url": html + "/src/branch/" + ref + "/" + e.Path, "git_url": repoURL + "/git/blobs/" + e.SHA,
		"download_url":      html + "/raw/branch/" + ref + "/" + e.Path,
		"submodule_git_url": nil,
		"_links":            map[string]any{"self": repoURL + "/contents/" + e.Path + "?ref=" + ref, "git": repoURL + "/git/blobs/" + e.SHA, "html": html + "/src/branch/" + ref + "/" + e.Path},
	}
	if withContent && e.Type == "blob" {
		b, _ := rp.blob(e.SHA)
		m["encoding"], m["content"] = "base64", base64.StdEncoding.EncodeToString(b)
	}
	return m
}

// getContents answers GET contents and contents/{path}: an object for a file, an array for a
// directory. ref is a branch, tag or commit SHA and defaults to the default branch.
func (f *Fake) getContents(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = rp.defaultBranch()
	}
	commit, ok := rp.resolve(ref)
	if !ok {
		return errf(http.StatusNotFound, "object does not exist [id: %s, rel_path: ]", ref)
	}
	p := strings.Trim(r.PathValue("path"), "/")
	notFound := errf(http.StatusNotFound, "object does not exist [id: %s, rel_path: %s]", ref, p)
	base := baseURL(r)
	if p == "" {
		es, err := rp.lsTree(commit, false)
		if err != nil {
			return err
		}
		out := []map[string]any{}
		for _, e := range es {
			out = append(out, f.contentEntry(base, rp, ref, commit, e, false))
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	e, ok := rp.entryAt(commit, p)
	if !ok {
		return notFound
	}
	if e.Type != "tree" {
		writeJSON(w, http.StatusOK, f.contentEntry(base, rp, ref, commit, e, true))
		return nil
	}
	children, err := rp.lsTree(commit+":"+p, false)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, c := range children {
		c.Path = p + "/" + c.Path
		out = append(out, f.contentEntry(base, rp, ref, commit, c, false))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// entryAt finds the tree entry at path in a commit.
func (rp *repo) entryAt(commit, p string) (entry, bool) {
	dir, name := path.Split(p)
	treeish := commit
	if dir != "" {
		treeish = commit + ":" + strings.TrimSuffix(dir, "/")
	}
	es, err := rp.lsTree(treeish, false)
	if err != nil {
		return entry{}, false
	}
	for _, e := range es {
		if e.Path == name {
			e.Path = p
			return e, true
		}
	}
	return entry{}, false
}

type changeFile struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	SHA       string `json:"sha"`
	FromPath  string `json:"from_path"`
}

type identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// changeFiles answers POST contents (ChangeFiles): one commit applying all files, on branch
// or on new_branch created from it.
func (f *Fake) changeFiles(w http.ResponseWriter, r *http.Request, rp *repo, login string) error {
	var body struct {
		Branch    string       `json:"branch"`
		NewBranch string       `json:"new_branch"`
		Message   string       `json:"message"`
		Author    identity     `json:"author"`
		Committer identity     `json:"committer"`
		Files     []changeFile `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid body: %v", err)
	}
	if len(body.Files) == 0 {
		return errf(http.StatusUnprocessableEntity, "[Files]: Required")
	}
	if body.Branch == "" {
		body.Branch = rp.defaultBranch()
	}
	if !rp.branchExists(body.Branch) {
		return errf(http.StatusNotFound, "branch does not exist [name: %s]", body.Branch)
	}
	if body.NewBranch != "" && rp.branchExists(body.NewBranch) {
		return errf(http.StatusUnprocessableEntity, "branch already exists [name: %s]", body.NewBranch)
	}
	parent, _ := rp.resolve(body.Branch)
	tree, err := rp.lsTree(parent, true)
	if err != nil {
		return err
	}
	existing := map[string]entry{}
	for _, e := range tree {
		existing[e.Path] = e
	}
	msg := body.Message
	if msg == "" {
		msg = "Update " + body.Files[0].Path
	}

	var ops []fileOp
	for _, cf := range body.Files {
		p, ok := cleanTreePath(cf.Path)
		if !ok {
			return errf(http.StatusUnprocessableEntity, "invalid file path [path: %s]", cf.Path)
		}
		content, err := base64.StdEncoding.DecodeString(cf.Content)
		if err != nil {
			return errf(http.StatusUnprocessableEntity, "content is not valid base64 [path: %s]", p)
		}
		op := cf.Operation
		if op == "" {
			return errf(http.StatusUnprocessableEntity, "[Operation]: Required")
		}
		cur, exists := existing[p]
		if exists && cur.Type != "blob" {
			return errf(http.StatusUnprocessableEntity, "path is a directory [path: %s]", p)
		}
		if op != "delete" {
			// A parent that is a file makes the path invalid (ErrFilePathInvalid).
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				if pe, ok := existing[d]; ok && pe.Type != "tree" {
					return errf(http.StatusUnprocessableEntity, "invalid file path, a parent is a file [path: %s]", p)
				}
			}
		}
		o := fileOp{op: op, path: p, content: content}
		if op == "upload" {
			// Create-or-update: an existing file is updated and keeps its mode.
			op = "create"
			if exists {
				op = "update"
			}
			o.op = op
		}
		switch op {
		case "create":
			if exists {
				return errf(http.StatusUnprocessableEntity, "repository file already exists [path: %s]", p)
			}
			existing[p] = entry{Type: "blob", Mode: "100644", Path: p}
		case "update":
			if !exists {
				return errf(http.StatusNotFound, "repository file does not exist [path: %s]", p)
			}
			if cf.Operation != "upload" && cf.SHA != "" && cf.SHA != cur.SHA {
				return errf(http.StatusUnprocessableEntity, "file does not match [path: %s, given: %s, expected: %s]", p, cf.SHA, cur.SHA)
			}
		case "delete":
			if !exists {
				return errf(http.StatusNotFound, "repository file does not exist [path: %s]", p)
			}
			// Gitea fills a missing sha with the branch head, so none is required.
			if cf.SHA != "" && cf.SHA != cur.SHA {
				return errf(http.StatusUnprocessableEntity, "file does not match [path: %s, given: %s, expected: %s]", p, cf.SHA, cur.SHA)
			}
			delete(existing, p)
		case "rename":
			from, ok := cleanTreePath(cf.FromPath)
			if !ok {
				return errf(http.StatusUnprocessableEntity, "invalid file path [path: %s]", cf.FromPath)
			}
			src, found := existing[from]
			if !found || src.Type != "blob" {
				return errf(http.StatusNotFound, "repository file does not exist [path: %s]", from)
			}
			if exists {
				return errf(http.StatusUnprocessableEntity, "repository file already exists [path: %s]", p)
			}
			o.fromPath = from
			if cf.Content == "" {
				o.content = nil
			}
			if cf.SHA != "" && cf.SHA != src.SHA {
				return errf(http.StatusUnprocessableEntity, "file does not match [path: %s, given: %s, expected: %s]", from, cf.SHA, src.SHA)
			}
			delete(existing, from)
			existing[p] = src
		default:
			return errf(http.StatusUnprocessableEntity, "unknown operation %q", op)
		}
		ops = append(ops, o)
	}

	now := f.now()
	who := identity{Name: login, Email: login + "@noreply.gitea.invalid"}
	pick := func(i identity, fallback identity) sig {
		if i.Name == "" {
			i = fallback
		}
		if i.Email == "" {
			i.Email = login + "@noreply.gitea.invalid"
		}
		return sig{Name: i.Name, Email: i.Email, When: now}
	}
	author := pick(body.Author, who)
	committer := pick(body.Committer, body.Author)
	if body.Committer.Name == "" && body.Author.Name == "" {
		committer = pick(who, who)
	}
	sha, err := rp.commit(parent, msg, author, committer, ops)
	if err != nil {
		return err
	}
	target, old := body.Branch, parent
	if body.NewBranch != "" {
		target, old = body.NewBranch, ""
	}
	if err := rp.setRef(target, sha, old); err != nil {
		return errf(http.StatusConflict, "branch changed concurrently: %v", err)
	}
	rp.calls["POST contents"]++

	c, _ := rp.commitInfo(sha)
	base := baseURL(r)
	files := []map[string]any{}
	for _, o := range ops {
		if o.op == "delete" {
			files = append(files, nil)
			continue
		}
		e, ok := rp.entryAt(sha, o.path)
		if !ok {
			continue
		}
		files = append(files, f.contentEntry(base, rp, target, sha, e, false))
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"files": files,
		"commit": map[string]any{
			"url": base + "/api/v1/repos/" + f.Owner + "/" + rp.name + "/git/commits/" + sha,
			"sha": sha, "html_url": base + "/" + f.Owner + "/" + rp.name + "/commit/" + sha,
			"author": f.gitUser(c.Author), "committer": f.gitUser(c.Committer), "message": c.Message,
			"tree":    map[string]any{"sha": c.Tree, "url": base + "/api/v1/repos/" + f.Owner + "/" + rp.name + "/git/trees/" + c.Tree},
			"parents": []map[string]any{{"sha": parent, "url": base + "/api/v1/repos/" + f.Owner + "/" + rp.name + "/git/commits/" + parent}},
			"created": ts(c.Committer.When),
		},
		"verification": map[string]any{"verified": false, "reason": "gpg.error.not_signed_commit", "signature": "", "payload": ""},
	})
	return nil
}

// cleanTreePath normalises a repository path like Gitea's CleanGitTreePath and rejects an empty
// path, a path escaping the root and any component equal to ".git" (case-insensitive).
func cleanTreePath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(strings.Trim(p, "/"), "..") {
		return "", false
	}
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "" {
		return "", false
	}
	for c := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(c, ".git") {
			return "", false
		}
	}
	return p, true
}
