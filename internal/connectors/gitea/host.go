// SPDX-License-Identifier: Apache-2.0

package gitea

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// HostID is the connector type ID of Gitea connections.
const HostID = "gitea"

// Setup-guide fields (and stored secret fields) of the two tokens.
const (
	ReadTokenField  = "readToken"
	WriteTokenField = "writeToken"
)

// maxTokenLength bounds a pasted token. Gitea's are 40 hex characters.
const maxTokenLength = 256

// Config is the non-secret part of a Gitea connection, stored in connections.config. Gitea does
// not expose when a token was created (the token API needs the user's password), so a token's age
// is the time since it was entered in Koinon (ADR 0063 open question). The fingerprints are the
// first bytes of a SHA-256 of each token, so that declaring the same token again keeps its age.
type Config struct {
	ReadTokenSetAt        time.Time `json:"readTokenSetAt"`
	WriteTokenSetAt       time.Time `json:"writeTokenSetAt"`
	ReadTokenFingerprint  string    `json:"readTokenFingerprint,omitempty"`
	WriteTokenFingerprint string    `json:"writeTokenFingerprint,omitempty"`
	// Site is the connection's server: its base URL (with an optional sub-path), CA bundle and
	// extra CI directories.
	codehost.Site
}

// ParseConfig decodes connections.config of a Gitea connection.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("connections: decode Gitea config: %w", err)
	}
	return c, nil
}

// Fingerprint is a short, non-secret identifier of a token value.
func Fingerprint(token []byte) string {
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:6])
}

// NewConfig builds the config of a connection with the given tokens. A token equal to the
// previous config's (same fingerprint) keeps its entry date, so re-declaring the same Secret
// does not make an old token look new.
func NewConfig(site codehost.Site, read, write []byte, prev *Config, now time.Time) Config {
	c := Config{
		Site: site, ReadTokenSetAt: now.UTC(), WriteTokenSetAt: now.UTC(),
		ReadTokenFingerprint: Fingerprint(read), WriteTokenFingerprint: Fingerprint(write),
	}
	if prev != nil {
		if prev.ReadTokenFingerprint == c.ReadTokenFingerprint && !prev.ReadTokenSetAt.IsZero() {
			c.ReadTokenSetAt = prev.ReadTokenSetAt
		}
		if prev.WriteTokenFingerprint == c.WriteTokenFingerprint && !prev.WriteTokenSetAt.IsZero() {
			c.WriteTokenSetAt = prev.WriteTokenSetAt
		}
	}
	return c
}

// Options configure the provider.
type Options struct {
	// HTTPClient is the base client for Gitea calls; default a client with a 30 s timeout that
	// dials through DialContext and returns a 3xx response instead of following it. A
	// connection's CA bundle derives a client from it.
	HTTPClient *http.Client
	// DialContext is the egress policy's dial check (catalog.URLPolicy.DialContext): a
	// Connection's base URL is admin-supplied. It is unused when HTTPClient is set. Nil dials
	// unchecked, for tests.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now defaults to time.Now; a poll's first cursor is taken from it.
	Now func() time.Time
}

// Host is Gitea as a code-host provider.
type Host struct {
	tr  *transports
	now func() time.Time
}

var _ codehost.Provider = (*Host)(nil)

// NewHost returns the Gitea provider.
func NewHost(o Options) *Host {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Host{tr: newTransports(o.HTTPClient, o.DialContext), now: o.Now}
}

// ID implements codehost.Provider.
func (*Host) ID() string { return HostID }

// Capabilities implements codehost.Provider: the descriptor is connector data.
func (*Host) Capabilities() codehost.Capabilities {
	t, ok := connectors.Get(HostID)
	if !ok {
		return codehost.Capabilities{DisplayName: "Gitea"}
	}
	c, err := codehost.CapabilitiesOf(t)
	if err != nil {
		return codehost.Capabilities{DisplayName: t.Metadata.DisplayName}
	}
	return c
}

func connectorType() (*connectors.Type, error) {
	t, ok := connectors.Get(HostID)
	if !ok {
		return nil, errors.New("gitea: the connector type is not embedded")
	}
	return t, nil
}

// location is where the connection's server lives, from its base URL.
func location(conn codehost.Connection) (connectors.Location, codehost.Site, error) {
	t, err := connectorType()
	if err != nil {
		return connectors.Location{}, codehost.Site{}, err
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return connectors.Location{}, codehost.Site{}, err
	}
	loc, err := site.Location(t)
	return loc, site, err
}

// ParseRepo implements codehost.Provider.
func (*Host) ParseRepo(full string) (codehost.Repo, error) {
	t, err := connectorType()
	if err != nil {
		return codehost.Repo{}, err
	}
	owner, name, err := t.SplitTarget(full)
	if err != nil {
		return codehost.Repo{}, fmt.Errorf("gitea: %w", err)
	}
	return codehost.Repo{Owner: owner, Name: name}, nil
}

// BranchURL implements codehost.Provider.
func (*Host) BranchURL(conn codehost.Connection, r codehost.Repo, branch string) string {
	loc, _, err := location(conn)
	if err != nil {
		return ""
	}
	return loc.BaseURL() + "/" + r.String() + "/src/branch/" + branch
}

// MetadataScope implements codehost.Provider: listing and naming repositories is a read.
func (*Host) MetadataScope(repositories []string) codehost.Scope {
	return codehost.Scope{Repositories: repositories, Permissions: map[string]string{"repository": "read"}}
}

// PollScope implements codehost.PollScoper: the issue comments, with the read token.
func (*Host) PollScope(res codehost.Resource) (codehost.Scope, error) {
	if res.Kind != codehost.ResourceIssueComments {
		return codehost.Scope{}, fmt.Errorf("%w: gitea %s", codehost.ErrResourceUnsupported, res.Kind)
	}
	return codehost.Scope{Repositories: []string{res.Repo.String()}, Permissions: map[string]string{"repository": "read"}}, nil
}

// Describe implements codehost.Provider.
func (*Host) Describe(config []byte) (codehost.Description, error) {
	c, err := ParseConfig(config)
	return codehost.Description{Site: c.Site, ReadTokenSetAt: c.ReadTokenSetAt, WriteTokenSetAt: c.WriteTokenSetAt}, err
}

func validToken(name string, b []byte) error {
	s := string(b)
	switch {
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("gitea: the %s is empty", name)
	case len(s) > maxTokenLength:
		return fmt.Errorf("gitea: the %s is longer than %d characters", name, maxTokenLength)
	case strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0:
		return fmt.Errorf("gitea: the %s holds spaces or control characters; paste only the token", name)
	}
	return nil
}

// ValidateSecrets implements codehost.Provider: both tokens, well formed and distinct.
func (*Host) ValidateSecrets(fields map[string][]byte) error {
	if err := validToken("read token", fields[ReadTokenField]); err != nil {
		return err
	}
	if err := validToken("write token", fields[WriteTokenField]); err != nil {
		return err
	}
	if string(fields[ReadTokenField]) == string(fields[WriteTokenField]) {
		return errors.New("gitea: the read token and the write token are the same; create two tokens, the read one with read scopes only")
	}
	return nil
}

// Validate implements codehost.Provider.
func (h *Host) Validate(conn codehost.Connection) error {
	if conn.Connector != HostID {
		return fmt.Errorf("connections: %s is a %s connection, not Gitea", conn.ID, conn.Connector)
	}
	if _, err := ParseConfig(conn.Config); err != nil {
		return err
	}
	if _, _, err := location(conn); err != nil {
		return err
	}
	return h.ValidateSecrets(conn.Fields)
}

// writes reports whether scope asks for any write permission, which selects the write token.
func writes(scope codehost.Scope) bool {
	for _, level := range scope.Permissions {
		if level == "write" {
			return true
		}
	}
	return false
}

func (h *Host) clientFor(conn codehost.Connection, token string) (*client, error) {
	loc, site, err := location(conn)
	if err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	hc, err := h.tr.clientFor(site.CABundle)
	if err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	return &client{base: loc.APIBaseURL(), token: token, http: hc}, nil
}

// Open implements codehost.Provider. A Gitea token cannot be narrowed per use (ADR 0063), so the
// scope only selects the token: the write token when any permission is "write", the read token
// otherwise. The bound repositories are enforced by the sandbox's network rules and by the
// approval's own target, not by the token.
func (h *Host) Open(_ context.Context, conn codehost.Connection, scope codehost.Scope) (codehost.Access, error) {
	field, kind := ReadTokenField, "read"
	if writes(scope) {
		field, kind = WriteTokenField, "write"
	}
	tok := conn.Fields[field]
	if err := validToken(kind+" token", tok); err != nil {
		return nil, fmt.Errorf("connections: connection %s: %w", conn.ID, err)
	}
	c, err := h.clientFor(conn, string(tok))
	if err != nil {
		return nil, err
	}
	t := codehost.NewToken(string(tok), time.Time{}, scope.Repositories)
	t.Revision = "gitea-" + kind + "-" + Fingerprint(tok)
	return &access{c: c, tok: t, now: h.now}, nil
}

type access struct {
	c   *client
	tok codehost.Token
	now func() time.Time
}

// errPush is returned by the writes that go by git push.
var errPush = errors.New("gitea: commits and branches are written by git push (internal/codehost/gitpush), not through the API")

func (a *access) Token() codehost.Token { return a.tok }

func (a *access) Repository(ctx context.Context, r codehost.Repo) (codehost.Repository, error) {
	got, err := a.c.repository(ctx, r)
	return got.repository(), err
}

// Repositories maps the caller's page onto Gitea's pages of at most 50.
func (a *access) Repositories(ctx context.Context, page, perPage int) (codehost.RepositoryPage, error) {
	if page < 1 {
		page = 1
	}
	perPage = max(1, min(perPage, codehost.MaxPerPage))
	start := (page - 1) * perPage
	first, last := start/maxPerPage+1, (start+perPage-1)/maxPerPage+1
	var all []apiRepo
	var total int64
	for p := first; p <= last; p++ {
		got, t, err := a.c.userRepos(ctx, p, maxPerPage)
		if err != nil {
			return codehost.RepositoryPage{}, err
		}
		total = t
		all = append(all, got...)
		if len(got) < maxPerPage {
			break
		}
	}
	skip := start - (first-1)*maxPerPage
	out := codehost.RepositoryPage{Total: total}
	for i := skip; i < len(all) && i < skip+perPage; i++ {
		out.Repositories = append(out.Repositories, all[i].repository())
	}
	return out, nil
}

func (a *access) Branch(ctx context.Context, r codehost.Repo, name string) (string, bool, error) {
	return a.c.branch(ctx, r, name)
}

func (a *access) Commit(ctx context.Context, r codehost.Repo, sha string) (codehost.Commit, error) {
	c, err := a.c.commit(ctx, r, sha)
	if err != nil {
		return codehost.Commit{}, err
	}
	out := codehost.Commit{SHA: c.SHA, Message: c.Commit.Message}
	for _, p := range c.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	return out, nil
}

func (a *access) File(ctx context.Context, r codehost.Repo, path, ref string) (codehost.File, bool, error) {
	return a.c.file(ctx, r, path, ref)
}

// maxTreePages bounds the pages read of a recursive listing (1000 entries each).
const maxTreePages = 20

// Modes returns the mode of each of paths that exists as a blob in commit's tree, from one
// recursive listing. A repository too large to list within maxTreePages is an error: guessing a
// mode would change the file's permissions silently.
func (a *access) Modes(ctx context.Context, r codehost.Repo, commit string, paths []string) (map[string]string, error) {
	modes := map[string]string{}
	if len(paths) == 0 {
		return modes, nil
	}
	c, err := a.c.commit(ctx, r, commit)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	for page := 1; ; page++ {
		t, err := a.c.tree(ctx, r, c.Commit.Tree.SHA, true, page)
		if err != nil {
			return nil, err
		}
		for _, e := range t.Tree {
			if e.Type == "blob" && want[e.Path] {
				modes[e.Path] = e.Mode
			}
		}
		if !t.Truncated || len(t.Tree) == 0 {
			return modes, nil
		}
		if page == maxTreePages {
			return nil, fmt.Errorf("gitea: %s is too large to read the modes of the changed files", r)
		}
	}
}

func (*access) WriteCommit(context.Context, codehost.Repo, codehost.CommitRequest) (string, error) {
	return "", errPush
}

func (*access) CreateBranch(context.Context, codehost.Repo, string, string) error { return errPush }

// maxPullPages bounds the pull requests scanned for a branch (50 per page, newest first).
const maxPullPages = 20

func (a *access) PullRequests(ctx context.Context, r codehost.Repo, branch string) ([]codehost.PullRequest, error) {
	var out []codehost.PullRequest
	for page := 1; page <= maxPullPages; page++ {
		got, err := a.c.pulls(ctx, r, "all", page, maxPerPage)
		if err != nil {
			return nil, err
		}
		for _, p := range got {
			if p.Head.Ref == branch {
				out = append(out, p.pullRequest())
			}
		}
		if len(got) < maxPerPage {
			break
		}
	}
	return out, nil
}

func (a *access) CreatePullRequest(ctx context.Context, r codehost.Repo, in codehost.PullRequestInput) (codehost.PullRequest, error) {
	p, err := a.c.createPull(ctx, r, in)
	return p.pullRequest(), err
}

const maxCommentPages = 20

func (a *access) Comments(ctx context.Context, r codehost.Repo, number int64) ([]codehost.Comment, error) {
	var out []codehost.Comment
	for page := 1; page <= maxCommentPages; page++ {
		got, err := a.c.comments(ctx, r, number, page, maxPerPage)
		if err != nil {
			return nil, err
		}
		for _, c := range got {
			out = append(out, codehost.Comment{ID: c.ID, URL: c.HTMLURL, Body: c.Body})
		}
		if len(got) < maxPerPage {
			break
		}
	}
	return out, nil
}

func (a *access) CreateComment(ctx context.Context, r codehost.Repo, number int64, body string) (codehost.Comment, error) {
	c, err := a.c.createComment(ctx, r, number, body)
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
