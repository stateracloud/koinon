// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// HostID is the connector type ID of GitHub connections. It is the provider's key in the
// code-host registry, the one place outside the connector data that names GitHub (ADR 0062).
const HostID = connectors.GitHubID

// PrivateKeyField is the setup-guide field (and stored secret field) of the App private key.
const PrivateKeyField = "privateKey"

// Config is the non-secret part of a GitHub connection, stored in connections.config under
// the setup guide's field IDs.
type Config struct {
	AppID          int64 `json:"appId"`
	InstallationID int64 `json:"installationId"`
	// Site is the connection's host when it is not github.com (GitHub Enterprise Server): its
	// base URL and CA bundle. Both are empty for github.com.
	codehost.Site
}

// ParseConfig decodes connections.config of a GitHub connection.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("connections: decode GitHub config: %w", err)
	}
	return c, nil
}

// AppOf builds the App of a connection from its config and its opened secret fields.
func AppOf(conn codehost.Connection) (App, error) {
	if conn.Connector != HostID {
		return App{}, fmt.Errorf("connections: %s is a %s connection, not GitHub", conn.ID, conn.Connector)
	}
	cfg, err := ParseConfig(conn.Config)
	if err != nil {
		return App{}, err
	}
	key, err := ParsePrivateKey(conn.Fields[PrivateKeyField])
	if err != nil {
		return App{}, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	return App{AppID: cfg.AppID, InstallationID: cfg.InstallationID, Key: key}, nil
}

// Host is GitHub as a code-host provider: the Client behind the codehost.Provider interface.
type Host struct{ c *Client }

// NewHost returns the GitHub provider using c; a nil c is a client for api.github.com.
func NewHost(c *Client) *Host {
	if c == nil {
		c = NewClient(Options{})
	}
	return &Host{c: c}
}

var _ codehost.Provider = (*Host)(nil)

// ID implements codehost.Provider.
func (*Host) ID() string { return HostID }

// Capabilities implements codehost.Provider: the descriptor is connector data.
func (*Host) Capabilities() codehost.Capabilities {
	t, ok := connectors.Get(HostID)
	if !ok {
		return codehost.Capabilities{DisplayName: "GitHub"}
	}
	c, err := codehost.CapabilitiesOf(t)
	if err != nil {
		return codehost.Capabilities{DisplayName: t.Metadata.DisplayName}
	}
	return c
}

// maxRepositories is the bound on repositories per token, from the connector data.
func maxRepositories() int {
	if t, ok := connectors.Get(HostID); ok && t.Spec.Capabilities != nil && t.Spec.Capabilities.MaxRepositories > 0 {
		return t.Spec.Capabilities.MaxRepositories
	}
	return MaxRepositories
}

// location is where the connection's host lives, from its base URL.
func location(conn codehost.Connection) (connectors.Location, codehost.Site, error) {
	t, ok := connectors.Get(HostID)
	if !ok {
		return connectors.Location{}, codehost.Site{}, errors.New("github: the connector type is not embedded")
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return connectors.Location{}, codehost.Site{}, err
	}
	loc, err := site.Location(t)
	return loc, site, err
}

// clientFor returns the client for the connection's host: the Host's own for github.com, one
// with the connection's API base URL and CA bundle otherwise.
func (h *Host) clientFor(conn codehost.Connection) (*Client, error) {
	loc, site, err := location(conn)
	if err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	apiBase := ""
	if !loc.Default {
		apiBase = loc.APIBaseURL()
	}
	c, err := h.c.For(apiBase, site.CABundle)
	if err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	return c, nil
}

// ParseRepo implements codehost.Provider.
func (*Host) ParseRepo(full string) (codehost.Repo, error) {
	r, err := ParseRepo(full)
	return codehost.Repo{Owner: r.Owner, Name: r.Name}, err
}

// BranchURL implements codehost.Provider.
func (*Host) BranchURL(conn codehost.Connection, r codehost.Repo, branch string) string {
	base := "https://github.com"
	if loc, _, err := location(conn); err == nil {
		base = loc.BaseURL()
	}
	return base + "/" + r.String() + "/tree/" + branch
}

// MetadataScope implements codehost.Provider.
func (*Host) MetadataScope(repositories []string) codehost.Scope {
	return codehost.Scope{Repositories: repositories, Permissions: map[string]string{"metadata": "read"}}
}

// PollScope implements codehost.PollScoper: the events feed needs metadata read, the issue
// comments issues read, each for the one repository.
func (*Host) PollScope(res codehost.Resource) (codehost.Scope, error) {
	repos := []string{res.Repo.String()}
	switch res.Kind {
	case codehost.ResourceRepoEvents:
		return codehost.Scope{Repositories: repos, Permissions: map[string]string{"metadata": "read"}}, nil
	case codehost.ResourceIssueComments:
		return codehost.Scope{Repositories: repos, Permissions: map[string]string{"metadata": "read", "issues": "read"}}, nil
	}
	return codehost.Scope{}, fmt.Errorf("%w: github %s", codehost.ErrResourceUnsupported, res.Kind)
}

// ValidateSecrets implements codehost.Provider.
func (*Host) ValidateSecrets(fields map[string][]byte) error {
	_, err := ParsePrivateKey(fields[PrivateKeyField])
	return err
}

// Validate implements codehost.Provider.
func (*Host) Validate(conn codehost.Connection) error {
	_, err := AppOf(conn)
	return err
}

// Describe implements codehost.Provider.
func (*Host) Describe(config []byte) (codehost.Description, error) {
	c, err := ParseConfig(config)
	return codehost.Description{AppID: c.AppID, InstallationID: c.InstallationID, Site: c.Site}, err
}

// Open implements codehost.Provider: it mints an installation token narrowed to scope.
func (h *Host) Open(ctx context.Context, conn codehost.Connection, scope codehost.Scope) (codehost.Access, error) {
	app, err := AppOf(conn)
	if err != nil {
		return nil, err
	}
	c, err := h.clientFor(conn)
	if err != nil {
		return nil, err
	}
	tok, err := c.InstallationToken(ctx, app, TokenRequest{Repositories: scope.Repositories, Permissions: scope.Permissions})
	if errors.Is(err, ErrRepositoryMismatch) {
		err = fmt.Errorf("%w: %w", codehost.ErrRepositoryMismatch, err)
	}
	if err != nil {
		return nil, err
	}
	return &access{c: c, tok: tok}, nil
}

type access struct {
	c   *Client
	tok Token
}

func repo(r codehost.Repo) Repo { return Repo{Owner: r.Owner, Name: r.Name} }

func (a *access) Token() codehost.Token {
	return codehost.NewToken(a.tok.Value(), a.tok.ExpiresAt, a.tok.Repositories)
}

func (a *access) Repository(ctx context.Context, r codehost.Repo) (codehost.Repository, error) {
	got, err := a.c.GetRepository(ctx, a.tok, repo(r))
	return codehost.Repository{FullName: got.FullName, DefaultBranch: got.DefaultBranch, Private: got.Private, Visibility: scopeVisibility(got)}, err
}

// scopeVisibility is the visibility Koinon records for a repository: private only when GitHub's
// visibility field says "private", public when it says "public", unknown otherwise. An internal
// repository is readable by the whole enterprise, and an older GitHub Enterprise Server that leaves
// the field out sends private=true for internal repositories too, so the private flag alone is not
// enough; an unsure answer must not hide a leak (ADR 0091).
func scopeVisibility(r Repository) string {
	switch r.Visibility {
	case "private":
		return codehost.VisibilityPrivate
	case "public":
		return codehost.VisibilityPublic
	}
	return codehost.VisibilityUnknown
}

func (a *access) Repositories(ctx context.Context, page, perPage int) (codehost.RepositoryPage, error) {
	got, err := a.c.ListInstallationRepositories(ctx, a.tok, page, perPage)
	if err != nil {
		return codehost.RepositoryPage{}, err
	}
	out := codehost.RepositoryPage{Total: got.TotalCount}
	for _, r := range got.Repositories {
		out.Repositories = append(out.Repositories, codehost.Repository{FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private, Visibility: scopeVisibility(r)})
	}
	return out, nil
}

func (a *access) Branch(ctx context.Context, r codehost.Repo, name string) (string, bool, error) {
	return a.c.GetBranch(ctx, a.tok, repo(r), name)
}

func (a *access) Commit(ctx context.Context, r codehost.Repo, sha string) (codehost.Commit, error) {
	c, err := a.c.GetCommit(ctx, a.tok, repo(r), sha)
	if err != nil {
		return codehost.Commit{}, err
	}
	out := codehost.Commit{SHA: c.SHA, Message: c.Message}
	for _, p := range c.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	return out, nil
}

func (a *access) File(ctx context.Context, r codehost.Repo, path, ref string) (codehost.File, bool, error) {
	f, found, err := a.c.GetFileContent(ctx, a.tok, repo(r), path, ref)
	return codehost.File{Type: f.Type, Size: f.Size, Content: f.Content}, found, err
}

// Modes returns the mode of each of paths that exists as a blob in commit's tree. One recursive
// listing answers it; if GitHub truncated that listing (a very large repository), the paths it
// did not return are looked up directory by directory. The calls are bounded by the 100-file
// cap of a pull request.
func (a *access) Modes(ctx context.Context, r codehost.Repo, commit string, paths []string) (map[string]string, error) {
	rp := repo(r)
	c, err := a.c.GetCommit(ctx, a.tok, rp, commit)
	if err != nil {
		return nil, err
	}
	tree := c.Tree.SHA
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	modes := map[string]string{}
	if len(want) == 0 {
		return modes, nil
	}
	all, err := a.c.GetTree(ctx, a.tok, rp, tree, true)
	if err != nil {
		return nil, err
	}
	for _, e := range all.Tree {
		if e.Type == "blob" && want[e.Path] {
			modes[e.Path] = e.Mode
		}
	}
	if !all.Truncated {
		return modes, nil
	}
	// dirs caches each directory's listing ("" is the root); nil when it does not exist.
	dirs := map[string][]TreeItem{}
	var list func(dir string) ([]TreeItem, error)
	list = func(dir string) ([]TreeItem, error) {
		if items, ok := dirs[dir]; ok {
			return items, nil
		}
		sha := tree
		if dir != "" {
			parent, name := path.Split(dir)
			items, err := list(strings.TrimSuffix(parent, "/"))
			if err != nil {
				return nil, err
			}
			sha = ""
			for _, e := range items {
				if e.Path == name && e.Type == "tree" {
					sha = e.SHA
				}
			}
			if sha == "" {
				dirs[dir] = nil
				return nil, nil
			}
		}
		t, err := a.c.GetTree(ctx, a.tok, rp, sha, false)
		if err != nil {
			return nil, err
		}
		dirs[dir] = t.Tree
		return t.Tree, nil
	}
	for p := range want {
		if _, ok := modes[p]; ok {
			continue
		}
		dir, name := path.Split(p)
		items, err := list(strings.TrimSuffix(dir, "/"))
		if err != nil {
			return nil, err
		}
		for _, e := range items {
			if e.Path == name && e.Type == "blob" {
				modes[p] = e.Mode
			}
		}
	}
	return modes, nil
}

// WriteCommit creates the blobs, the tree on top of the base commit's tree and the commit,
// with the Git Data API.
func (a *access) WriteCommit(ctx context.Context, r codehost.Repo, req codehost.CommitRequest) (string, error) {
	rp := repo(r)
	base, err := a.c.GetCommit(ctx, a.tok, rp, req.Base)
	if err != nil {
		return "", err
	}
	entries := make([]TreeEntry, 0, len(req.Files))
	for _, f := range req.Files {
		e := TreeEntry{Path: f.Path, Mode: f.Mode, Type: "blob"}
		if !f.Delete {
			sha, err := a.c.CreateBlob(ctx, a.tok, rp, f.Content)
			if err != nil {
				return "", err
			}
			e.SHA = &sha
		}
		entries = append(entries, e)
	}
	tree, err := a.c.CreateTree(ctx, a.tok, rp, base.Tree.SHA, entries)
	if err != nil {
		return "", err
	}
	return a.c.CreateCommit(ctx, a.tok, rp, req.Message, tree, []string{req.Base})
}

func (a *access) CreateBranch(ctx context.Context, r codehost.Repo, name, sha string) error {
	err := a.c.CreateBranch(ctx, a.tok, repo(r), name, sha)
	if errors.Is(err, ErrRefExists) {
		return codehost.ErrBranchExists
	}
	return err
}

func pullRequest(p PullRequest) codehost.PullRequest {
	return codehost.PullRequest{Number: p.Number, URL: p.HTMLURL, State: p.State, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref}
}

func (a *access) PullRequests(ctx context.Context, r codehost.Repo, branch string) ([]codehost.PullRequest, error) {
	prs, err := a.c.ListPullRequestsForBranch(ctx, a.tok, repo(r), branch)
	if err != nil {
		return nil, err
	}
	out := make([]codehost.PullRequest, 0, len(prs))
	for _, p := range prs {
		out = append(out, pullRequest(p))
	}
	return out, nil
}

func (a *access) CreatePullRequest(ctx context.Context, r codehost.Repo, in codehost.PullRequestInput) (codehost.PullRequest, error) {
	p, err := a.c.CreatePullRequest(ctx, a.tok, repo(r), in.Title, in.Body, in.Head, in.Base)
	return pullRequest(p), err
}

func (a *access) Comments(ctx context.Context, r codehost.Repo, number int64) ([]codehost.Comment, error) {
	cs, err := a.c.ListIssueComments(ctx, a.tok, repo(r), number)
	if err != nil {
		return nil, err
	}
	out := make([]codehost.Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, codehost.Comment{ID: c.ID, URL: c.HTMLURL, Body: c.Body})
	}
	return out, nil
}

func (a *access) CreateComment(ctx context.Context, r codehost.Repo, number int64, body string) (codehost.Comment, error) {
	c, err := a.c.CreateIssueComment(ctx, a.tok, repo(r), number, body)
	return codehost.Comment{ID: c.ID, URL: c.HTMLURL, Body: c.Body}, err
}

// CheckNames implements codehost.Provider.
func (*Host) CheckNames() []string {
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
