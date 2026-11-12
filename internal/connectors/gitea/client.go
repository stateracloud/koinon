// SPDX-License-Identifier: Apache-2.0

// Package gitea is the Gitea code-host provider (ADR 0062, 0063): a REST client for the endpoints
// Koinon uses, the codehost.Provider built on it, and the connection checks of the setup guide.
//
// A Gitea connection is one bot account with two personal access tokens, held in the secret
// store. The READ token is the credential for read scopes: it is what the reconciler pushes into
// an OpenShell provider record, so a sandbox only ever sees a placeholder. The WRITE token is
// used by koinon-api alone, through the push writer (internal/codehost/gitpush) and the pull
// request and comment calls after an approval. Neither token is logged or put into an error.
//
// Gitea cannot write a commit with exact file modes through its API (ADR 0062), so this
// provider's Access reads and opens pull requests; commits and branches are pushed by gitpush,
// which wraps it because the connector data says writes are "push".
package gitea

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/codehost"
)

// maxPerPage is Gitea's default maximum page size (MAX_RESPONSE_ITEMS = 50).
const maxPerPage = 50

// client talks to one Gitea server's REST API with one token.
type client struct {
	base  string // https://host[/sub]/api/v1, no trailing slash
	token string
	http  *http.Client
}

// APIError is a non-success response from Gitea.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("gitea: %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// HTTPStatus implements codehost.StatusError.
func (e *APIError) HTTPStatus() int { return e.Status }

// Detail implements codehost.StatusError.
func (e *APIError) Detail() string { return e.Message }

var _ codehost.StatusError = (*APIError)(nil)

// do sends one request and decodes a JSON answer into out (nil to discard). It returns the
// response headers.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	full := c.base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries the URL, which holds no credentials.
		return nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("gitea: %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: truncate(e.Message, 300)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("gitea: %s %s: decode response: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// escapeRepo is owner/name, path-escaped.
func escapeRepo(r codehost.Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// newHTTPClient returns the default client: it connects only through dial (when set), ignores the
// environment's proxy (a proxy would take the traffic around the dial check) and returns a 3xx
// response instead of following it, so a Connection's admin-supplied URL cannot send Koinon on to
// another host. It mirrors the trace exporter's client (internal/tracing/export).
func newHTTPClient(dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		tr.DialContext = dial
		tr.Proxy = nil
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: noRedirect}
}

// noRedirect makes the client return the 3xx response itself.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// transports caches one HTTP client per CA bundle, so connections share their connection pools.
type transports struct {
	base *http.Client
	m    sync.Map // sha256 of the bundle -> *http.Client
}

func newTransports(base *http.Client, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *transports {
	if base == nil {
		base = newHTTPClient(dial, 30*time.Second)
	}
	return &transports{base: base}
}

// clientFor returns the HTTP client for a connection's CA bundle: the base client when there is none.
func (t *transports) clientFor(caBundle string) (*http.Client, error) {
	if strings.TrimSpace(caBundle) == "" {
		return t.base, nil
	}
	sum := sha256.Sum256([]byte(caBundle))
	key := hex.EncodeToString(sum[:])
	if c, ok := t.m.Load(key); ok {
		return c.(*http.Client), nil
	}
	pool, err := codehost.CertPool(caBundle)
	if err != nil {
		return nil, fmt.Errorf("gitea: %w", err)
	}
	hc := *t.base
	hc.CheckRedirect = noRedirect
	var tr *http.Transport
	switch x := hc.Transport.(type) {
	case nil:
		tr = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		tr = x.Clone()
	default:
		return nil, errors.New("gitea: the HTTP client's transport cannot take a CA bundle")
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	hc.Transport = tr
	got, _ := t.m.LoadOrStore(key, &hc)
	return got.(*http.Client), nil
}

// Wire shapes: the fields Koinon reads of Gitea's v1 API.

type apiUser struct {
	Login string `json:"login"`
}

type apiPermissions struct {
	Admin bool `json:"admin"`
	Push  bool `json:"push"`
	Pull  bool `json:"pull"`
}

type apiRepo struct {
	FullName      string          `json:"full_name"`
	DefaultBranch string          `json:"default_branch"`
	Private       bool            `json:"private"`
	HasIssues     bool            `json:"has_issues"`
	Permissions   *apiPermissions `json:"permissions"`
}

// repository maps the wire shape. Only a repository Gitea marks private is VisibilityPrivate: a
// public repository, an internal one (public in a private organization) and one in a limited
// organization all report public, because each is readable beyond the repository's collaborators
// (ADR 0091: when unsure, public).
func (r apiRepo) repository() codehost.Repository {
	vis := codehost.VisibilityPublic
	if r.Private {
		vis = codehost.VisibilityPrivate
	}
	return codehost.Repository{FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private, Visibility: vis}
}

type apiPull struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p apiPull) pullRequest() codehost.PullRequest {
	return codehost.PullRequest{Number: p.Number, URL: p.HTMLURL, State: p.State, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref}
}

type apiComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

func (c *client) version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

func (c *client) user(ctx context.Context) (apiUser, error) {
	var u apiUser
	_, err := c.do(ctx, http.MethodGet, "/user", nil, nil, &u)
	return u, err
}

func (c *client) repository(ctx context.Context, r codehost.Repo) (apiRepo, error) {
	var out apiRepo
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r), nil, nil, &out)
	return out, err
}

// userRepos lists a page of the repositories the token's user can see (1-based, at most 50 per
// page) and the total from X-Total-Count.
func (c *client) userRepos(ctx context.Context, page, limit int) ([]apiRepo, int64, error) {
	var out []apiRepo
	h, err := c.do(ctx, http.MethodGet, "/user/repos", url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}, nil, &out)
	if err != nil {
		return nil, 0, err
	}
	total, _ := strconv.ParseInt(h.Get("X-Total-Count"), 10, 64)
	if total < int64((page-1)*limit+len(out)) {
		total = int64((page-1)*limit + len(out))
	}
	return out, total, nil
}

func (c *client) branch(ctx context.Context, r codehost.Repo, name string) (string, bool, error) {
	var out struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/branches/"+escapePath(name), nil, nil, &out)
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out.Commit.ID, true, nil
}

// escapePath escapes each segment of a slash-separated path.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

type apiCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

func (c *client) commit(ctx context.Context, r codehost.Repo, sha string) (apiCommit, error) {
	var out apiCommit
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/git/commits/"+url.PathEscape(sha), nil, nil, &out)
	return out, err
}

func (c *client) file(ctx context.Context, r codehost.Repo, path, ref string) (codehost.File, bool, error) {
	var raw json.RawMessage
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/contents/"+escapePath(strings.Trim(path, "/")), q, nil, &raw)
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return codehost.File{}, false, nil
	}
	if err != nil {
		return codehost.File{}, false, err
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return codehost.File{Type: "dir"}, true, nil
	}
	var f struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return codehost.File{}, false, fmt.Errorf("gitea: decode contents: %w", err)
	}
	out := codehost.File{Type: f.Type, Size: f.Size}
	if f.Content != "" && f.Encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return codehost.File{}, false, fmt.Errorf("gitea: decode file content: %w", err)
		}
		out.Content = b
	}
	return out, true, nil
}

type apiTree struct {
	Tree []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
	} `json:"tree"`
	Truncated  bool  `json:"truncated"`
	TotalCount int64 `json:"total_count"`
}

func (c *client) tree(ctx context.Context, r codehost.Repo, sha string, recursive bool, page int) (apiTree, error) {
	var out apiTree
	q := url.Values{"per_page": {"1000"}, "page": {strconv.Itoa(page)}}
	if recursive {
		q.Set("recursive", "true")
	}
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/git/trees/"+url.PathEscape(sha), q, nil, &out)
	return out, err
}

func (c *client) pulls(ctx context.Context, r codehost.Repo, state string, page, limit int) ([]apiPull, error) {
	var out []apiPull
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/pulls", url.Values{"state": {state}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}, nil, &out)
	return out, err
}

func (c *client) createPull(ctx context.Context, r codehost.Repo, in codehost.PullRequestInput) (apiPull, error) {
	var out apiPull
	_, err := c.do(ctx, http.MethodPost, escapeRepo(r)+"/pulls", nil, map[string]string{
		"title": in.Title, "body": in.Body, "head": in.Head, "base": in.Base,
	}, &out)
	return out, err
}

func (c *client) comments(ctx context.Context, r codehost.Repo, number int64, page, limit int) ([]apiComment, error) {
	var out []apiComment
	_, err := c.do(ctx, http.MethodGet, escapeRepo(r)+"/issues/"+strconv.FormatInt(number, 10)+"/comments",
		url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}, nil, &out)
	return out, err
}

func (c *client) createComment(ctx context.Context, r codehost.Repo, number int64, body string) (apiComment, error) {
	var out apiComment
	_, err := c.do(ctx, http.MethodPost, escapeRepo(r)+"/issues/"+strconv.FormatInt(number, 10)+"/comments", nil, map[string]string{"body": body}, &out)
	return out, err
}

// probeWrite sends a write to a repository that Gitea must refuse before it reads the request
// when the token has no write scope: creating an issue with an empty body. A token that can
// write answers 422 (the title is missing) and nothing is created. It returns the status.
func (c *client) probeWrite(ctx context.Context, r codehost.Repo) (int, error) {
	_, err := c.do(ctx, http.MethodPost, escapeRepo(r)+"/issues", nil, map[string]string{}, nil)
	var api *APIError
	if errors.As(err, &api) {
		return api.Status, nil
	}
	if err != nil {
		return 0, err
	}
	// 2xx for an empty issue: Gitea created something. Report it as writable.
	return http.StatusCreated, nil
}
