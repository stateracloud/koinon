// SPDX-License-Identifier: Apache-2.0

// Package codehost is the seam between Koinon and a code host such as GitHub (ADR 0062).
//
// A Provider is one implementation per host. It parses repository names, opens an Access
// narrowed to some repositories (the credential is minted or loaded by the provider), runs the
// connection's setup-guide checks, and describes what its credential can and cannot narrow
// (Capabilities). Access is everything Koinon calls on a repository: reads for the approval
// card and the reconciler, and the writes that run after an approval (a commit with exact file
// modes, a branch, a pull request, a comment).
//
// Callers look a provider up in a Registry by the connector type ID of the connection or the
// approval request. Nothing outside the provider registry names a particular provider.
package codehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stateracloud/koinon/connectors"
)

// Repo names a repository as the provider spells it. Owner and Name are enough for the
// providers of v0.1; a provider with deeper paths (ADR 0062 open questions) widens Owner.
type Repo struct{ Owner, Name string }

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Connection is a stored connection with its secret fields opened. Fields hold secrets; never
// log or marshal a Connection.
type Connection struct {
	ID string
	// DisplayName is the connection's name, for messages.
	DisplayName string
	// Connector is the connector type ID of the connection.
	Connector string
	// Config is the non-secret JSON the provider defined for its connections.
	Config []byte
	// Fields are the opened secret fields, by setup-guide field ID.
	Fields map[string][]byte
}

// Scope is what one use of a credential is narrowed to. Permissions are named as the provider
// names them (the permission map of the connector type's scopes), "read" or "write".
type Scope struct {
	Repositories []string
	Permissions  map[string]string
}

// Token is a credential minted for a Scope. It formats and logs without its value.
type Token struct {
	value        string
	ExpiresAt    time.Time
	Repositories []string
	// Revision names the credential without revealing it, for a token that does not expire (a
	// static token has a zero ExpiresAt): it changes when the token does, so the reconciler
	// knows to push a new value.
	Revision string
}

// NewToken returns a Token holding value.
func NewToken(value string, expiresAt time.Time, repositories []string) Token {
	return Token{value: value, ExpiresAt: expiresAt, Repositories: repositories}
}

// Value returns the token for an Authorization header. Never log it.
func (t Token) Value() string { return t.value }

// String implements fmt.Stringer without revealing the token.
func (t Token) String() string {
	if t.ExpiresAt.IsZero() {
		return fmt.Sprintf("code host token (revision %s, %d repositories)", t.Revision, len(t.Repositories))
	}
	return fmt.Sprintf("code host token (expires %s, %d repositories)", t.ExpiresAt.Format(time.RFC3339), len(t.Repositories))
}

// GoString implements fmt.GoStringer without revealing the token.
func (t Token) GoString() string { return t.String() }

// LogValue implements slog.LogValuer without revealing the token.
func (t Token) LogValue() slog.Value {
	if t.ExpiresAt.IsZero() {
		return slog.GroupValue(slog.String("revision", t.Revision), slog.Any("repositories", t.Repositories))
	}
	return slog.GroupValue(slog.Time("expires_at", t.ExpiresAt), slog.Any("repositories", t.Repositories))
}

// Repository is what Koinon reads of a repository.
type Repository struct {
	// FullName is the repository's name in the provider's spelling (a renamed repository
	// answers under its new name).
	FullName      string
	DefaultBranch string
	Private       bool
}

// DefaultMaxRepositories is the bound on the repositories of one grant when the provider
// states none.
const DefaultMaxRepositories = 500

// MaxPerPage is the largest page Access.Repositories serves.
const MaxPerPage = 100

// RepositoryPage is one page of the repositories a connection can access.
type RepositoryPage struct {
	// Total is how many repositories the credential can access, over all pages.
	Total        int64
	Repositories []Repository
}

// Commit is a commit object.
type Commit struct {
	SHA     string
	Message string
	// Parents are the parents' SHAs, first parent first.
	Parents []string
}

// File is a path at a ref.
type File struct {
	// Type is "file", "dir", "symlink" or "submodule".
	Type string
	Size int64
	// Content is nil when the provider sent none (a large file, or anything that is not a
	// file).
	Content []byte
}

// FileWrite is one path of a commit.
type FileWrite struct {
	Path string
	// Mode is the git mode the file is written with ("100644", "100755"). Ignored for a delete.
	Mode    string
	Content []byte
	Delete  bool
}

// CommitRequest is one commit on top of Base with exactly Files applied.
type CommitRequest struct {
	// Base is the SHA of the parent commit.
	Base    string
	Message string
	Files   []FileWrite
}

// PullRequest is the part of a pull request Koinon reads.
type PullRequest struct {
	Number  int64
	URL     string
	State   string
	HeadRef string
	BaseRef string
}

// PullRequestInput is a pull request to open.
type PullRequestInput struct {
	Title, Body, Head, Base string
}

// Comment is an issue or pull request comment.
type Comment struct {
	ID   int64
	URL  string
	Body string
}

// Access is one provider credential narrowed to a Scope, and everything Koinon does with it
// on a repository. Errors that carry an HTTP status satisfy Status.
type Access interface {
	// Token returns the credential, for the sandbox's provider record.
	Token() Token

	Repository(ctx context.Context, r Repo) (Repository, error)
	// Repositories lists the repositories the credential can access (1-based pages).
	Repositories(ctx context.Context, page, perPage int) (RepositoryPage, error)
	// Branch returns the commit a branch points at; found is false when it does not exist.
	Branch(ctx context.Context, r Repo, name string) (sha string, found bool, err error)
	Commit(ctx context.Context, r Repo, sha string) (Commit, error)
	// File returns path at ref; found is false when it does not exist there.
	File(ctx context.Context, r Repo, path, ref string) (File, bool, error)
	// Modes returns the git mode of each of paths that exists as a blob at commit, keyed by
	// path; an absent path is new.
	Modes(ctx context.Context, r Repo, commit string, paths []string) (map[string]string, error)

	// Commit writes one commit and returns its SHA. It creates no branch.
	WriteCommit(ctx context.Context, r Repo, req CommitRequest) (string, error)
	// CreateBranch creates a branch at sha; ErrBranchExists if it exists.
	CreateBranch(ctx context.Context, r Repo, name, sha string) error
	// PullRequests returns the pull requests, open or closed, whose head is branch of r.
	PullRequests(ctx context.Context, r Repo, branch string) ([]PullRequest, error)
	CreatePullRequest(ctx context.Context, r Repo, in PullRequestInput) (PullRequest, error)
	Comments(ctx context.Context, r Repo, number int64) ([]Comment, error)
	CreateComment(ctx context.Context, r Repo, number int64, body string) (Comment, error)
}

// CheckResult is one setup-guide check's outcome. Message never contains secrets.
type CheckResult struct {
	Check   string `json:"check"`
	Step    string `json:"step"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Description is the non-secret part of a connection's stored config, for the connection page.
type Description struct {
	AppID          int64
	InstallationID int64
	// Site is the connection's host when it is not the connector's default.
	Site Site
	// Token-based providers (Gitea, ADR 0063): when each token was entered in Koinon (the
	// provider does not expose its creation date). The tokens never leave the secret store.
	ReadTokenSetAt, WriteTokenSetAt time.Time
}

// Capabilities says what a provider's credential and write API can and cannot narrow, so risk
// tiers, approvals and the UI can compensate (ADR 0062, 0063). The fields are a starting point
// (ADR 0062 open questions). The descriptor is data: connector.yaml's spec.capabilities,
// spec.ciPaths and auth.tokenLifetime, read by CapabilitiesOf.
type Capabilities struct {
	// DisplayName is how messages name the provider.
	DisplayName string
	// CredentialClass is "app-installation" for a token minted per use, "static-token" for a
	// long-lived bot token.
	CredentialClass string
	// TokenLifetime is zero for a credential that does not expire.
	TokenLifetime time.Duration
	// TokenWarnAfter is the age of a static token after which Koinon warns; zero for none. It
	// never blocks and never rotates (ADR 0063).
	TokenWarnAfter time.Duration
	// NarrowsRepositories and NarrowsPermissions say whether one use of the credential can
	// be limited to the call's repositories and permissions.
	NarrowsRepositories bool
	NarrowsPermissions  bool
	// MaxRepositories bounds the repositories one credential can be narrowed to; zero when it
	// cannot narrow.
	MaxRepositories int
	// CanDenyCI is whether the credential can refuse changes to CI files.
	CanDenyCI bool
	// ExactModes is whether WriteCommit sets exact file modes through the provider's API.
	ExactModes bool
	// Writes is how commits reach the host: WritesAPI (the default when empty) through the
	// provider's API, or WritesPush by git push from koinon-api (ADR 0062; gitpush wraps the
	// provider's Access).
	Writes string
	// CIPaths are the path prefixes whose changes run CI, which the workflow guard refuses.
	CIPaths []string
	// CIPathsFoldCase is whether the provider's file system ignores case, so the guard matches
	// CIPaths without regard to case. False (exact) unless the connector data says so.
	CIPathsFoldCase bool
}

// Values of Capabilities.Writes.
const (
	WritesAPI  = "api"
	WritesPush = "push"
)

// Provider is one code host.
type Provider interface {
	// ID is the connector type ID of the provider's connections.
	ID() string
	Capabilities() Capabilities
	// ParseRepo parses and validates a repository name as bound to an agent.
	ParseRepo(full string) (Repo, error)
	// BranchURL is the page of a branch on the connection's host.
	BranchURL(conn Connection, r Repo, branch string) string
	// MetadataScope is the least scope that lists and names repositories, for connection checks
	// and for validating bindings. Empty repositories means every repository the connection
	// can access.
	MetadataScope(repositories []string) Scope
	// Open mints (or loads) a credential for scope and returns the Access using it.
	Open(ctx context.Context, conn Connection, scope Scope) (Access, error)
	// Checks runs the connection's setup-guide checks in step order; after the first failure
	// the rest are reported as skipped.
	Checks(ctx context.Context, conn Connection, t *connectors.Type) []CheckResult
	// CheckNames are the checks the provider implements.
	CheckNames() []string
	// Describe decodes the connection's non-secret config.
	Describe(config []byte) (Description, error)
	// ValidateSecrets reports whether the setup-guide secret fields are well formed.
	ValidateSecrets(fields map[string][]byte) error
	// Validate reports whether the whole opened connection (config and secrets) is usable, so a
	// malformed stored connection is reported as such rather than as an unreachable host.
	Validate(conn Connection) error
}

// Errors of Access.
var (
	// ErrBranchExists is returned by CreateBranch when the branch already exists.
	ErrBranchExists = errors.New("codehost: the branch already exists")
	// ErrRepositoryMismatch means the credential minted for some repositories does not cover
	// exactly the requested ones.
	ErrRepositoryMismatch = errors.New("codehost: repositories do not match the request")
)

// StatusError is implemented by errors that carry the host's HTTP status.
type StatusError interface {
	error
	HTTPStatus() int
	// Detail is the host's own message; it never contains credentials.
	Detail() string
}

// Status returns the HTTP status of the host's answer in err, or 0 when err carries none.
func Status(err error) int {
	var s StatusError
	if errors.As(err, &s) {
		return s.HTTPStatus()
	}
	return 0
}

// IsStatus reports whether err is a host answer with one of the statuses.
func IsStatus(err error, status ...int) bool {
	got := Status(err)
	for _, s := range status {
		if got != 0 && got == s {
			return true
		}
	}
	return false
}

// AsStatus returns the StatusError in err.
func AsStatus(err error) (StatusError, bool) {
	var s StatusError
	if errors.As(err, &s) {
		return s, true
	}
	return nil, false
}
