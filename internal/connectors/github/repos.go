// SPDX-License-Identifier: Apache-2.0

package github

// Repository calls with an installation token, for executing approved ask-tier calls (plan
// item P1, ADR 0049): Koinon, never the agent, creates the branch, the commit and the pull
// request, through the Git Data API so the commit holds exactly the approved files.
//
// Upstream references, checked 2026-10-22:
//
//   - GET  /repos/{owner}/{repo}                       default_branch
//     https://docs.github.com/en/rest/repos/repos#get-a-repository
//   - GET  /repos/{owner}/{repo}/contents/{path}?ref=  200 {type, size, encoding, content}
//     (base64; encoding "none" and no content above 1 MB; an array for a directory) or 404
//     https://docs.github.com/en/rest/repos/contents#get-repository-content
//   - GET  /repos/{owner}/{repo}/git/ref/{ref}         200 or 404 (ref is heads/<branch>)
//   - POST /repos/{owner}/{repo}/git/refs              201; 422 "Reference already exists"
//     https://docs.github.com/en/rest/git/refs
//   - GET  /repos/{owner}/{repo}/git/commits/{sha}     tree.sha, message, parents
//   - POST /repos/{owner}/{repo}/git/commits           201 {message, tree, parents}
//     https://docs.github.com/en/rest/git/commits
//   - POST /repos/{owner}/{repo}/git/blobs             201 {content, encoding: base64}
//     https://docs.github.com/en/rest/git/blobs
//   - POST /repos/{owner}/{repo}/git/trees             201 {base_tree, tree: [{path, mode,
//     type, sha}]}; an entry with sha null deletes the path
//     https://docs.github.com/en/rest/git/trees
//   - GET  /repos/{owner}/{repo}/pulls?head=owner:branch&state=all
//   - POST /repos/{owner}/{repo}/pulls                 201 {title, body, head, base}
//     https://docs.github.com/en/rest/pulls/pulls
//   - GET  /repos/{owner}/{repo}/issues/{n}/comments   per_page up to 100, page
//   - POST /repos/{owner}/{repo}/issues/{n}/comments   201 {body}
//     https://docs.github.com/en/rest/issues/comments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Repo names a repository.
type Repo struct{ Owner, Name string }

// ParseRepo parses owner/name.
func ParseRepo(full string) (Repo, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("github: repository %q must be owner/name", full)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

func (r Repo) path(format string, args ...any) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + fmt.Sprintf(format, args...)
}

// IsStatus reports whether err is an APIError with one of the statuses.
func IsStatus(err error, status ...int) bool {
	var e *APIError
	return errors.As(err, &e) && slices.Contains(status, e.Status)
}

// withToken calls the REST API with an installation token. A status outside want is an
// *APIError.
func (c *Client) withToken(ctx context.Context, tok Token, method, path string, body, out any, want ...int) error {
	if tok.value == "" {
		return errors.New("github: no installation token")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("Authorization", "Bearer "+tok.value)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("github: %s %s: read response: %w", method, path, err)
	}
	if !slices.Contains(want, resp.StatusCode) {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: truncate(e.Message, 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Repository is the part of GET /repos/{owner}/{repo} Koinon reads.
type Repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// GetRepository returns the repository.
func (c *Client) GetRepository(ctx context.Context, tok Token, r Repo) (Repository, error) {
	var out Repository
	err := c.withToken(ctx, tok, http.MethodGet, r.path(""), nil, &out, http.StatusOK)
	return out, err
}

// MaxPerPage is the largest page GitHub's list endpoints serve.
const MaxPerPage = 100

// InstallationRepositories is one page of GET /installation/repositories.
type InstallationRepositories struct {
	// TotalCount is how many repositories the installation can access, on every page.
	TotalCount   int64        `json:"total_count"`
	Repositories []Repository `json:"repositories"`
}

// ListInstallationRepositories returns one page (1-based, perPage up to MaxPerPage) of the
// repositories the token's installation can access; a token narrowed to some repositories
// lists only those.
//
//   - GET /installation/repositories?per_page=&page=   {total_count, repositories}
//     https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation
func (c *Client) ListInstallationRepositories(ctx context.Context, tok Token, page, perPage int) (InstallationRepositories, error) {
	if page < 1 || perPage < 1 || perPage > MaxPerPage {
		return InstallationRepositories{}, fmt.Errorf("github: page must be at least 1 and per_page 1 to %d", MaxPerPage)
	}
	var out InstallationRepositories
	path := "/installation/repositories?" + url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}.Encode()
	err := c.withToken(ctx, tok, http.MethodGet, path, nil, &out, http.StatusOK)
	return out, err
}

// GetBranch returns the commit SHA a branch points at; found is false when it does not
// exist.
func (c *Client) GetBranch(ctx context.Context, tok Token, r Repo, branch string) (sha string, found bool, err error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err = c.withToken(ctx, tok, http.MethodGet, r.path("/git/ref/heads/%s", escapeRef(branch)), nil, &out, http.StatusOK)
	if IsStatus(err, http.StatusNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out.Object.SHA, true, nil
}

// FileContent is a path at a ref, from the contents API.
type FileContent struct {
	// Type is "file", "dir", "symlink" or "submodule".
	Type string
	Size int64
	// Content is the file's bytes; nil when GitHub sent none (files above 1 MB, and
	// anything that is not a file).
	Content []byte
}

// GetFileContent returns path at ref (a branch, tag or commit SHA); found is false when
// the path does not exist there. It needs contents:read.
func (c *Client) GetFileContent(ctx context.Context, tok Token, r Repo, path, ref string) (FileContent, bool, error) {
	var raw json.RawMessage
	q := url.Values{"ref": {ref}}
	err := c.withToken(ctx, tok, http.MethodGet, r.path("/contents/%s?%s", escapeRef(path), q.Encode()), nil, &raw, http.StatusOK)
	if IsStatus(err, http.StatusNotFound) {
		return FileContent{}, false, nil
	}
	if err != nil {
		return FileContent{}, false, err
	}
	if len(raw) > 0 && raw[0] == '[' {
		return FileContent{Type: "dir"}, true, nil
	}
	var out struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return FileContent{}, false, fmt.Errorf("github: contents of %s: decode response: %w", path, err)
	}
	fc := FileContent{Type: out.Type, Size: out.Size}
	if out.Type == "file" && out.Encoding == "base64" {
		// GitHub wraps the base64 in lines.
		b, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(out.Content))
		if err != nil {
			return FileContent{}, false, fmt.Errorf("github: contents of %s: decode base64: %w", path, err)
		}
		fc.Content = b
	}
	return fc, true, nil
}

// escapeRef escapes each segment of a branch name, keeping the slashes.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Commit is a git commit object.
type Commit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// GetCommit returns a commit object.
func (c *Client) GetCommit(ctx context.Context, tok Token, r Repo, sha string) (Commit, error) {
	var out Commit
	err := c.withToken(ctx, tok, http.MethodGet, r.path("/git/commits/%s", url.PathEscape(sha)), nil, &out, http.StatusOK)
	return out, err
}

// CreateBlob stores content byte for byte and returns its SHA.
func (c *Client) CreateBlob(ctx context.Context, tok Token, r Repo, content []byte) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/git/blobs"), map[string]string{
		"content": base64.StdEncoding.EncodeToString(content), "encoding": "base64",
	}, &out, http.StatusCreated)
	return out.SHA, err
}

// TreeItem is one entry of an existing tree: a blob (Mode 100644, 100755 or 120000), a
// subtree (040000) or a submodule commit (160000).
type TreeItem struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// Tree is a tree listing. Truncated is set when GitHub cut a recursive listing short (more
// than 100,000 entries or 7 MB); the entries it did return are correct.
type Tree struct {
	SHA       string     `json:"sha"`
	Tree      []TreeItem `json:"tree"`
	Truncated bool       `json:"truncated"`
}

// GetTree lists a tree: its direct entries, or with recursive every entry below it, with
// paths relative to the tree.
func (c *Client) GetTree(ctx context.Context, tok Token, r Repo, sha string, recursive bool) (Tree, error) {
	p := r.path("/git/trees/%s", url.PathEscape(sha))
	if recursive {
		p += "?recursive=1"
	}
	var out Tree
	err := c.withToken(ctx, tok, http.MethodGet, p, nil, &out, http.StatusOK)
	return out, err
}

// TreeEntry is one path of a new tree. A nil SHA deletes the path from the base tree.
type TreeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}

// CreateTree creates a tree from baseTree with entries applied, and returns its SHA.
func (c *Client) CreateTree(ctx context.Context, tok Token, r Repo, baseTree string, entries []TreeEntry) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/git/trees"), map[string]any{
		"base_tree": baseTree, "tree": entries,
	}, &out, http.StatusCreated)
	return out.SHA, err
}

// CreateCommit creates a commit object and returns its SHA. The author and committer are the
// App the token belongs to.
func (c *Client) CreateCommit(ctx context.Context, tok Token, r Repo, message, tree string, parents []string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/git/commits"), map[string]any{
		"message": message, "tree": tree, "parents": parents,
	}, &out, http.StatusCreated)
	return out.SHA, err
}

// ErrRefExists is returned by CreateBranch when the branch already exists.
var ErrRefExists = errors.New("github: the branch already exists")

// CreateBranch creates refs/heads/<branch> at sha.
func (c *Client) CreateBranch(ctx context.Context, tok Token, r Repo, branch, sha string) error {
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/git/refs"), map[string]string{
		"ref": "refs/heads/" + branch, "sha": sha,
	}, nil, http.StatusCreated)
	if IsStatus(err, http.StatusUnprocessableEntity) {
		return ErrRefExists
	}
	return err
}

// PullRequest is the part of a pull request Koinon reads.
type PullRequest struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// ListPullRequestsForBranch returns the repository's pull requests, open or closed, whose
// head is branch of the same repository.
func (c *Client) ListPullRequestsForBranch(ctx context.Context, tok Token, r Repo, branch string) ([]PullRequest, error) {
	q := url.Values{"head": {r.Owner + ":" + branch}, "state": {"all"}, "per_page": {"100"}}
	var out []PullRequest
	err := c.withToken(ctx, tok, http.MethodGet, r.path("/pulls?%s", q.Encode()), nil, &out, http.StatusOK)
	return out, err
}

// CreatePullRequest opens a pull request from head into base.
func (c *Client) CreatePullRequest(ctx context.Context, tok Token, r Repo, title, body, head, base string) (PullRequest, error) {
	var out PullRequest
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/pulls"), map[string]any{
		"title": title, "body": body, "head": head, "base": base, "maintainer_can_modify": false,
	}, &out, http.StatusCreated)
	return out, err
}

// IssueComment is the part of an issue comment Koinon reads.
type IssueComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

// maxCommentPages bounds the idempotency lookup of ListIssueComments (100 per page).
const maxCommentPages = 30

// ListIssueComments returns up to 3000 comments of an issue or pull request, oldest first.
func (c *Client) ListIssueComments(ctx context.Context, tok Token, r Repo, number int64) ([]IssueComment, error) {
	var all []IssueComment
	for page := 1; page <= maxCommentPages; page++ {
		var out []IssueComment
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.withToken(ctx, tok, http.MethodGet, r.path("/issues/%d/comments?%s", number, q.Encode()), nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < 100 {
			break
		}
	}
	return all, nil
}

// CreateIssueComment comments on an issue or pull request.
func (c *Client) CreateIssueComment(ctx context.Context, tok Token, r Repo, number int64, body string) (IssueComment, error) {
	var out IssueComment
	err := c.withToken(ctx, tok, http.MethodPost, r.path("/issues/%d/comments", number), map[string]string{"body": body}, &out, http.StatusCreated)
	return out, err
}
