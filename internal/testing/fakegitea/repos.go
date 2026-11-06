// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"encoding/json"
	"net/http"
	"strings"
)

func userJSON(base, login string) map[string]any {
	return map[string]any{
		"id": 1, "login": login, "login_name": "", "source_id": 0, "full_name": "", "email": login + "@noreply.gitea.invalid",
		"avatar_url": base + "/avatars/" + login, "html_url": base + "/" + login, "language": "", "is_admin": false,
		"restricted": false, "active": true, "prohibit_login": false, "visibility": "public", "username": login,
	}
}

func (f *Fake) repoJSON(base string, rp *repo) map[string]any {
	return f.repoJSONFor(base, rp, "write")
}

// repoJSONFor is repoJSON as seen by a login with the given permission on the repository.
func (f *Fake) repoJSONFor(base string, rp *repo, permission string) map[string]any {
	html := base + "/" + f.Owner + "/" + rp.name
	return map[string]any{
		"id": rp.id, "owner": userJSON(base, f.Owner), "name": rp.name, "full_name": f.Owner + "/" + rp.name,
		"description": "", "empty": false, "private": f.public[strings.ToLower(rp.name)] == "", "fork": false, "template": false, "mirror": false,
		"size": 1, "html_url": html, "ssh_url": "git@" + "gitea.invalid:" + f.Owner + "/" + rp.name + ".git",
		"clone_url": html + ".git", "original_url": "", "website": "", "stars_count": 0, "forks_count": 0,
		"watchers_count": 0, "open_issues_count": len(rp.issues), "open_pr_counter": rp.openPulls(),
		"default_branch": rp.defaultBranch(), "archived": false, "internal": f.public[strings.ToLower(rp.name)] == "internal",
		"has_issues": true, "has_pull_requests": true, "has_code": true,
		"created_at": ts(rp.created), "updated_at": ts(rp.created),
		"permissions":         map[string]bool{"admin": permission == "admin", "push": canWrite(permission), "pull": permission != ""},
		"allow_merge_commits": true, "allow_rebase": true, "allow_squash_merge": true,
		"default_merge_style": "merge",
	}
}

func (rp *repo) openPulls() int {
	n := 0
	for _, p := range rp.pulls {
		if p.state == "open" {
			n++
		}
	}
	return n
}

func (f *Fake) listRepos(w http.ResponseWriter, r *http.Request, login string) error {
	var all []map[string]any
	for _, rp := range f.sortedRepos() {
		if perm := f.permission(login, rp.name); perm != "" {
			all = append(all, f.repoJSONFor(baseURL(r), rp, perm))
		}
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, all)))
	return nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (f *Fake) getRepo(w http.ResponseWriter, r *http.Request, rp *repo, login string) error {
	writeJSON(w, http.StatusOK, f.repoJSONFor(baseURL(r), rp, f.permission(login, rp.name)))
	return nil
}

func (f *Fake) commitSummary(base string, rp *repo, c gitCommit) map[string]any {
	person := func(s sig) map[string]any { return map[string]any{"name": s.Name, "email": s.Email, "username": ""} }
	return map[string]any{
		"id": c.SHA, "message": c.Message, "url": base + "/" + f.Owner + "/" + rp.name + "/commit/" + c.SHA,
		"author": person(c.Author), "committer": person(c.Committer), "verification": nil,
		"timestamp": ts(c.Committer.When), "added": nil, "removed": nil, "modified": nil,
	}
}

func (f *Fake) branchJSON(base string, rp *repo, name, sha string) map[string]any {
	c, _ := rp.commitInfo(sha)
	return map[string]any{
		"name": name, "commit": f.commitSummary(base, rp, c), "protected": false, "required_approvals": 0,
		"enable_status_check": false, "status_check_contexts": []string{}, "user_can_push": true,
		"user_can_merge": true, "effective_branch_protection_name": "",
	}
}

func (f *Fake) listBranches(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	bs, err := rp.branches()
	if err != nil {
		return err
	}
	var all []map[string]any
	for _, b := range bs {
		all = append(all, f.branchJSON(baseURL(r), rp, b[0], b[1]))
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, all)))
	return nil
}

func (f *Fake) getBranch(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	name := r.PathValue("branch")
	if !rp.branchExists(name) {
		return errf(http.StatusNotFound, "branch does not exist [name: %s]", name)
	}
	sha, _ := rp.resolve(name)
	writeJSON(w, http.StatusOK, f.branchJSON(baseURL(r), rp, name, sha))
	return nil
}

func (f *Fake) createBranch(w http.ResponseWriter, r *http.Request, rp *repo, _ string) error {
	var body struct {
		NewBranchName string `json:"new_branch_name"`
		OldRefName    string `json:"old_ref_name"`
		OldBranchName string `json:"old_branch_name"` // deprecated, still honoured by Gitea 28.2
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid body: %v", err)
	}
	if body.NewBranchName == "" {
		return errf(http.StatusUnprocessableEntity, "[NewBranchName]: Required")
	}
	if body.OldRefName == "" {
		body.OldRefName = body.OldBranchName
	}
	if body.OldRefName == "" {
		body.OldRefName = rp.defaultBranch()
	}
	sha, ok := rp.resolve(body.OldRefName)
	if !ok {
		return errf(http.StatusNotFound, "The target couldn't be found.")
	}
	if rp.branchExists(body.NewBranchName) {
		return errf(http.StatusConflict, "branch already exists [name: %s]", body.NewBranchName)
	}
	if err := rp.setRef(body.NewBranchName, sha, ""); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid branch name: %v", err)
	}
	rp.calls["POST branches"]++
	writeJSON(w, http.StatusCreated, f.branchJSON(baseURL(r), rp, body.NewBranchName, sha))
	return nil
}
