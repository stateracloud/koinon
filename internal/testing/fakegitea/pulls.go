// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (f *Fake) refJSON(base string, rp *repo, branch, sha string) map[string]any {
	return map[string]any{
		"label": branch, "ref": branch, "sha": sha, "repo_id": rp.id, "repo": f.repoJSON(base, rp),
	}
}

func (f *Fake) pullJSON(base string, rp *repo, p *pull) map[string]any {
	html := base + "/" + f.Owner + "/" + rp.name + "/pulls/" + strconv.FormatInt(p.index, 10)
	// head moves with the branch, as it does in Gitea.
	head := p.headSHA
	if sha, ok := rp.resolve(p.head); ok {
		head = sha
	}
	return map[string]any{
		"id": p.index + 1000, "url": html, "number": p.index, "user": userJSON(base, p.user), "title": p.title,
		"body": p.body, "labels": []any{}, "milestone": nil, "assignee": nil, "assignees": nil,
		"state": p.state, "is_locked": false, "comments": len(rp.commentsOf(p.index)), "html_url": html,
		"diff_url": html + ".diff", "patch_url": html + ".patch", "mergeable": true, "merged": false,
		"merged_at": nil, "merge_commit_sha": nil, "merged_by": nil, "allow_maintainer_edit": false,
		"base": f.refJSON(base, rp, p.base, p.baseSHA), "head": f.refJSON(base, rp, p.head, head),
		"merge_base": p.baseSHA, "due_date": nil, "created_at": ts(p.created), "updated_at": ts(p.created),
		"closed_at": nil, "pin_order": 0,
	}
}

func (rp *repo) commentsOf(index int64) []*comment {
	var res []*comment
	for _, c := range rp.comments {
		if c.index == index {
			res = append(res, c)
		}
	}
	return res
}

func (f *Fake) listPulls(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	var all []map[string]any
	for _, p := range slices.Backward(rp.pulls) {
		if state == "all" || p.state == state {
			all = append(all, f.pullJSON(baseURL(r), rp, p))
		}
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, all)))
	return nil
}

func (f *Fake) getPull(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	idx, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err == nil {
		for _, p := range rp.pulls {
			if p.index == idx {
				writeJSON(w, http.StatusOK, f.pullJSON(baseURL(r), rp, p))
				return nil
			}
		}
	}
	return errf(http.StatusNotFound, "issue does not exist [id: 0, repo_id: %d, index: %s]", rp.id, r.PathValue("index"))
}

// stripOwner drops a leading "<owner>:" from a head, which GitHub clients write.
func (f *Fake) stripOwner(head string) string {
	if o, b, ok := strings.Cut(head, ":"); ok && strings.EqualFold(o, f.Owner) {
		return b
	}
	return head
}

// getPullByBaseHead answers GET pulls/{base}/{head}: the open pull request between two
// branches, else the most recent one.
func (f *Fake) getPullByBaseHead(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	base, head := r.PathValue("base"), f.stripOwner(r.PathValue("head"))
	var found *pull
	for _, p := range rp.pulls {
		if p.base == base && p.head == head && (found == nil || found.state != "open") {
			found = p
		}
	}
	if found == nil {
		return errf(http.StatusNotFound, "pull request does not exist [repo_id: %d, head: %s, base: %s]", rp.id, head, base)
	}
	writeJSON(w, http.StatusOK, f.pullJSON(baseURL(r), rp, found))
	return nil
}

func (f *Fake) createPull(w http.ResponseWriter, r *http.Request, rp *repo, login string) error {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid body: %v", err)
	}
	head := f.stripOwner(body.Head)
	if body.Title == "" {
		return errf(http.StatusUnprocessableEntity, "[Title]: Required")
	}
	if head == "" {
		return errf(http.StatusUnprocessableEntity, "[Head]: Required")
	}
	if body.Base == "" {
		return errf(http.StatusUnprocessableEntity, "[Base]: Required")
	}
	headSHA, ok := rp.branchHead(head)
	if !ok {
		return errf(http.StatusNotFound, "head branch does not exist [name: %s]", head)
	}
	baseSHA, ok := rp.branchHead(body.Base)
	if !ok {
		return errf(http.StatusNotFound, "base branch does not exist [name: %s]", body.Base)
	}
	for _, p := range rp.pulls {
		if p.state == "open" && p.head == head && p.base == body.Base {
			return errf(http.StatusConflict, "pull request already exists for these targets [id: %d, head: %s, base: %s]", p.index+1000, head, body.Base)
		}
	}
	if headSHA == baseSHA {
		return errf(http.StatusUnprocessableEntity, "no changes between head and base [head: %s, base: %s]", head, body.Base)
	}
	p := &pull{
		issue: issue{index: rp.nextIndex, title: body.Title, body: body.Body, user: login, created: f.now()},
		head:  head, base: body.Base, headSHA: headSHA, baseSHA: baseSHA, state: "open",
	}
	rp.nextIndex++
	rp.pulls = append(rp.pulls, p)
	rp.calls["POST pulls"]++
	writeJSON(w, http.StatusCreated, f.pullJSON(baseURL(r), rp, p))
	return nil
}

// exists reports whether an issue or pull request has this index.
func (rp *repo) exists(index int64) bool {
	if _, ok := rp.issues[index]; ok {
		return true
	}
	for _, p := range rp.pulls {
		if p.index == index {
			return true
		}
	}
	return false
}

func (f *Fake) commentJSON(base string, rp *repo, c *comment) map[string]any {
	idx := strconv.FormatInt(c.index, 10)
	repoURL := base + "/" + f.Owner + "/" + rp.name
	return map[string]any{
		"id": c.id, "html_url": repoURL + "/issues/" + idx + "#issuecomment-" + strconv.FormatInt(c.id, 10),
		"pull_request_url": repoURL + "/pulls/" + idx, "issue_url": repoURL + "/issues/" + idx,
		"user": userJSON(base, c.user), "original_author": "", "original_author_id": 0, "body": c.body,
		"assets": []any{}, "created_at": ts(c.created), "updated_at": ts(c.created),
	}
}

func (f *Fake) commentIndex(r *http.Request, rp *repo) (int64, error) {
	idx, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err != nil || !rp.exists(idx) {
		return 0, errf(http.StatusNotFound, "issue does not exist [id: 0, repo_id: %d, index: %s]", rp.id, r.PathValue("index"))
	}
	return idx, nil
}

// listComments answers GET issues/{index}/comments. Like Gitea's it has no pagination, only
// the since and before filters.
func (f *Fake) listComments(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	idx, err := f.commentIndex(r, rp)
	if err != nil {
		return err
	}
	parse := func(k string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339, r.URL.Query().Get(k))
		return t, err == nil
	}
	since, hasSince := parse("since")
	before, hasBefore := parse("before")
	out := []map[string]any{}
	for _, c := range rp.commentsOf(idx) {
		if (hasSince && c.created.Before(since)) || (hasBefore && !c.created.Before(before)) {
			continue
		}
		out = append(out, f.commentJSON(baseURL(r), rp, c))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (f *Fake) createComment(w http.ResponseWriter, r *http.Request, rp *repo, login string) error {
	idx, err := f.commentIndex(r, rp)
	if err != nil {
		return err
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid body: %v", err)
	}
	if body.Body == "" {
		return errf(http.StatusUnprocessableEntity, "[Body]: Required")
	}
	rp.nextCID++
	c := &comment{id: rp.nextCID, index: idx, user: login, body: body.Body, created: f.now()}
	rp.comments = append(rp.comments, c)
	rp.calls["POST comments"]++
	writeJSON(w, http.StatusCreated, f.commentJSON(baseURL(r), rp, c))
	return nil
}

// createIssue answers POST issues. Only what the setup-guide check needs: a title is required
// (422 otherwise, after the scope and permission checks), and an issue is created.
func (f *Fake) createIssue(w http.ResponseWriter, r *http.Request, rp *repo, login string) error {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid body: %v", err)
	}
	if body.Title == "" {
		return errf(http.StatusUnprocessableEntity, "[Title]: Required")
	}
	i := &issue{index: rp.nextIndex, title: body.Title, body: body.Body, user: login, created: f.now()}
	rp.nextIndex++
	rp.issues[i.index] = i
	rp.calls["POST issues"]++
	html := baseURL(r) + "/" + f.Owner + "/" + rp.name + "/issues/" + strconv.FormatInt(i.index, 10)
	writeJSON(w, http.StatusCreated, map[string]any{"id": i.index + 2000, "number": i.index, "title": i.title, "body": i.body, "state": "open", "html_url": html})
	return nil
}

// listRepoComments answers GET issues/comments: every comment of the repository, oldest first,
// filtered by since and before and paginated with page and limit as Gitea's is. Like Gitea's it
// sends no ETag and no Last-Modified. Each GET counts as "GET repo comments" in Snapshot.Calls.
func (f *Fake) listRepoComments(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	parse := func(k string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339, r.URL.Query().Get(k))
		return t, err == nil
	}
	since, hasSince := parse("since")
	before, hasBefore := parse("before")
	var all []map[string]any
	for _, c := range rp.comments {
		if (hasSince && c.created.Before(since)) || (hasBefore && !c.created.Before(before)) {
			continue
		}
		all = append(all, f.commentJSON(baseURL(r), rp, c))
	}
	rp.calls["GET repo comments"]++
	out := paginate(w, r, all)
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
