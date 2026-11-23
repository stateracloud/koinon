// SPDX-License-Identifier: Apache-2.0

// Package githublink links a Koinon user to their GitHub login (issue #1175, ADR 0005 amendment
// 2026-11-23). The user authorizes the org's own GitHub App once. Koinon reads the login and the
// numeric user ID with the user token, revokes the token at once and keeps only the login. With
// the link, Koinon asks the requester to review an agent pull request; the App still authors it.
//
// A link belongs to one GitHub host (github.com or a GitHub Enterprise Server). Creating and
// removing a link are audited (github_link.create, github_link.delete). The user removes their own
// link; an org admin removes any link of the org.
package githublink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
)

// CallbackPath is where GitHub sends the user back. It is under /auth/ like the other browser
// flows, so the web dev server forwards it to the API.
const CallbackPath = "/auth/github/link/callback"

// FlowTTL is how long a started link stays valid.
const FlowTTL = 10 * time.Minute

// The page the callback sends the browser back to, with the result in the query.
const returnPage = "/connections"

var (
	// ErrNotGitHub means the connection is not a GitHub connection.
	ErrNotGitHub = errors.New("githublink: only a GitHub connection links users")
	// ErrNoPublicURL means Koinon has no public URL, so GitHub cannot send the user back.
	ErrNoPublicURL = errors.New("githublink: Koinon has no public URL; set it to link GitHub users")
	// ErrNotFound means no such link.
	ErrNotFound = errors.New("githublink: no such link")
	// ErrNoUserAuth means the connection's App cannot sign users in (github.ErrNoUserAuth).
	ErrNoUserAuth = github.ErrNoUserAuth
)

// Linker is the part of the GitHub provider a link needs. *github.Host has it.
type Linker interface {
	WebURL(conn codehost.Connection) (string, error)
	AuthorizeURL(conn codehost.Connection, redirectURI, state, challenge string) (string, error)
	LinkUser(ctx context.Context, conn codehost.Connection, code, redirectURI, verifier string) (github.User, error)
}

// Authenticator finds the signed-in user of a browser request. *auth.Service has it.
type Authenticator interface {
	AuthenticateRequest(r *http.Request) (*auth.Principal, error)
}

// Options configure a Service.
type Options struct {
	// Keys opens the connection's stored secret fields (the App's client secret).
	Keys connections.Opener
	// Hosts are the code-host providers; the "github" one must implement Linker.
	Hosts *codehost.Registry
	// PublicURL is Koinon's external URL; the callback is under it.
	PublicURL string
	// Auth authenticates the callback's browser session.
	Auth   Authenticator
	Now    func() time.Time
	Logger *slog.Logger
}

// Service creates, lists and removes links.
type Service struct {
	pool *pgxpool.Pool
	q    *store.Queries
	opts Options
}

// New returns a Service.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	return &Service{pool: pool, q: store.New(pool), opts: opts}
}

// Link is one user's link on one host.
type Link struct {
	UserID       pgtype.UUID
	Host         string
	GitHubUserID int64
	GitHubLogin  string
	CreatedAt    time.Time
	// DisplayName and Email are set by List only.
	DisplayName, Email string
}

func (s *Service) redirectURI() string { return s.opts.PublicURL + CallbackPath }

// PublicURL is Koinon's public URL, or "" when the operator did not set one.
func (s *Service) PublicURL() string { return s.opts.PublicURL }

// linker returns the GitHub provider and the loaded connection.
func (s *Service) linker(ctx context.Context, org, connID pgtype.UUID) (Linker, codehost.Connection, error) {
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: connID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, codehost.Connection{}, ErrNotFound
	}
	if err != nil {
		return nil, codehost.Connection{}, err
	}
	if row.Connector != github.HostID {
		return nil, codehost.Connection{}, ErrNotGitHub
	}
	p, ok := s.opts.Hosts.Get(row.Connector)
	l, isLinker := p.(Linker)
	if !ok || !isLinker {
		return nil, codehost.Connection{}, ErrNotGitHub
	}
	conn, err := connections.Load(ctx, s.opts.Keys, row)
	if err != nil {
		return nil, codehost.Connection{}, err
	}
	return l, conn, nil
}

// Start begins a link for the user through the connection's App and returns the GitHub page to
// send the browser to. The caller has checked that the user is a member of the org.
func (s *Service) Start(ctx context.Context, org, user, connID pgtype.UUID) (string, error) {
	l, conn, err := s.linker(ctx, org, connID)
	if err != nil {
		return "", err
	}
	if s.opts.PublicURL == "" {
		return "", ErrNoPublicURL
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	// The state names the org, so the callback knows where to look; only its hash is stored.
	state := org.String() + "." + base64.RawURLEncoding.EncodeToString(secret)
	verifier := oauth2.GenerateVerifier()
	authURL, err := l.AuthorizeURL(conn, s.redirectURI(), state, oauth2.S256ChallengeFromVerifier(verifier))
	if err != nil {
		return "", err
	}
	now := s.opts.Now()
	if err := s.q.DeleteExpiredGitHubLinkFlows(ctx, store.DeleteExpiredGitHubLinkFlowsParams{OrgID: org, Now: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(state))
	err = s.q.CreateGitHubLinkFlow(ctx, store.CreateGitHubLinkFlowParams{
		OrgID: org, StateHash: h[:], UserID: user, ConnectionID: connID, CodeVerifier: verifier,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(FlowTTL), Valid: true},
	})
	if err != nil {
		return "", err
	}
	return authURL, nil
}

// Result is the outcome of a callback, given to the web page as ?github_link=<result>.
type Result string

// The results of a callback.
const (
	ResultLinked    Result = "linked"
	ResultSignIn    Result = "signin"  // no Koinon session
	ResultExpired   Result = "expired" // unknown, used or expired state, or another user's
	ResultDenied    Result = "denied"  // the user did not authorize the App
	ResultTaken     Result = "taken"   // the GitHub account is linked to another user
	ResultFailed    Result = "failed"  // GitHub or Koinon failed
	resultParamName        = "github_link"
)

// Mount registers the callback handler.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+CallbackPath, s.Callback)
}

// Callback is where GitHub sends the browser back. It always redirects to the connections page
// with the result; details go to the log, never to the browser.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	res := s.callback(r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, returnPage+"?"+url.Values{resultParamName: {string(res)}}.Encode(), http.StatusSeeOther)
}

func (s *Service) callback(r *http.Request) Result {
	ctx := r.Context()
	p, err := s.opts.Auth.AuthenticateRequest(r)
	if err != nil || p == nil {
		return ResultSignIn
	}
	q := r.URL.Query()
	state := q.Get("state")
	orgPart, _, ok := strings.Cut(state, ".")
	var org pgtype.UUID
	if !ok || org.Scan(orgPart) != nil || !org.Valid {
		return ResultExpired
	}
	h := sha256.Sum256([]byte(state))
	flow, err := s.q.ConsumeGitHubLinkFlow(ctx, store.ConsumeGitHubLinkFlowParams{
		OrgID: org, StateHash: h[:], UserID: p.UserID, Now: pgtype.Timestamptz{Time: s.opts.Now(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultExpired
	}
	if err != nil {
		s.opts.Logger.ErrorContext(ctx, "github link: read the flow", "org", org.String(), "error", err)
		return ResultFailed
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		return ResultDenied
	}
	l, conn, err := s.linker(ctx, org, flow.ConnectionID)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "github link: load the connection", "org", org.String(), "connection", flow.ConnectionID.String(), "error", err)
		return ResultFailed
	}
	host, err := l.WebURL(conn)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "github link: host", "org", org.String(), "error", err)
		return ResultFailed
	}
	u, err := l.LinkUser(ctx, conn, q.Get("code"), s.redirectURI(), flow.CodeVerifier)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "github link: GitHub refused the sign-in", "org", org.String(), "error", err)
		return ResultFailed
	}
	if err := s.save(ctx, org, p.UserID, host, u); err != nil {
		if isUnique(err) {
			return ResultTaken
		}
		s.opts.Logger.ErrorContext(ctx, "github link: save", "org", org.String(), "error", err)
		return ResultFailed
	}
	return ResultLinked
}

// save writes the link and its audit event in one transaction.
func (s *Service) save(ctx context.Context, org, user pgtype.UUID, host string, u github.User) error {
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		var before any // nil: no "before" for a first link
		old, err := q.GetGitHubUserLink(ctx, store.GetGitHubUserLinkParams{OrgID: org, UserID: user, Host: host})
		switch {
		case err == nil:
			before = linkFields(old)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		row, err := q.UpsertGitHubUserLink(ctx, store.UpsertGitHubUserLinkParams{
			OrgID: org, UserID: user, Host: host, GithubUserID: u.ID, GithubLogin: u.Login,
		})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: audit.User(user), Action: "github_link.create", TargetKind: "user", TargetID: user.String(),
			Before: before, After: linkFields(row),
		})
		return nil
	})
	return err
}

func linkFields(l store.GithubUserLink) map[string]any {
	return map[string]any{"host": l.Host, "githubLogin": l.GithubLogin, "githubUserId": l.GithubUserID}
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// Mine returns the user's links in the org.
func (s *Service) Mine(ctx context.Context, org, user pgtype.UUID) ([]Link, error) {
	rows, err := s.q.ListGitHubUserLinksOfUser(ctx, store.ListGitHubUserLinksOfUserParams{OrgID: org, UserID: user})
	if err != nil {
		return nil, err
	}
	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		out = append(out, Link{UserID: r.UserID, Host: r.Host, GitHubUserID: r.GithubUserID, GitHubLogin: r.GithubLogin, CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

// List returns every link of the org, with the users' names, for an admin.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Link, error) {
	rows, err := s.q.ListGitHubUserLinks(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		out = append(out, Link{
			UserID: r.UserID, Host: r.Host, GitHubUserID: r.GithubUserID, GitHubLogin: r.GithubLogin, CreatedAt: r.CreatedAt.Time,
			DisplayName: r.DisplayName, Email: r.Email.String,
		})
	}
	return out, nil
}

// Delete removes the user's link on the host. The caller has checked that actor is the user or
// an org admin.
func (s *Service) Delete(ctx context.Context, org, user pgtype.UUID, host string, actor audit.Actor) error {
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		row, err := q.DeleteGitHubUserLink(ctx, store.DeleteGitHubUserLinkParams{OrgID: org, UserID: user, Host: host})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "github_link.delete", TargetKind: "user", TargetID: user.String(),
			Before: linkFields(row),
		})
		return nil
	})
	return err
}

// Login returns the user's linked login on the host, and false when there is none.
func Login(ctx context.Context, q *store.Queries, org, user pgtype.UUID, host string) (string, bool, error) {
	if !user.Valid || host == "" {
		return "", false, nil
	}
	row, err := q.GetGitHubUserLink(ctx, store.GetGitHubUserLinkParams{OrgID: org, UserID: user, Host: host})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("githublink: read the link: %w", err)
	}
	return row.GithubLogin, true, nil
}
