// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// The repo.read grant in the sandbox (plan item C2). An agent whose approved grant on the
// GitHub connector includes a scope with network rules gets:
//
//   - a provider record holding an installation token narrowed to the bound repositories with
//     read-only permissions, behind a stable placeholder in GITHUB_TOKEN and GH_TOKEN. Every
//     pass asks the token cache for a token; once one is within its refresh margin of expiry
//     (github.DefaultRefreshMargin, 10 of its 60 minutes) a new one is minted and pushed into
//     the same record, which running processes see without a restart (spike 2.7);
//   - a per-agent provider profile whose endpoints are the grant's network rules for those
//     repositories, so the token is only ever sent there (OpenShell binds a static credential
//     to its profile's endpoints);
//   - the same endpoints as the sandbox policy entry github_read, enforced, so the grant is
//     visible in the sandbox policy and updated in place when the repositories change.
//
// What OpenShell 0.1.7 cannot express is listed in connectors/README.md.
const (
	githubRule       = "github_read"
	githubCredential = "github_token"
)

// GitHubTokenEnvVars carry the token's placeholder in the sandbox: git credential helpers and
// gh read GITHUB_TOKEN or GH_TOKEN.
var GitHubTokenEnvVars = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// DefaultGitBinaries may use the GitHub endpoints besides the flavor's own binaries. git's
// git-remote-https helper matches as a child of git.
var DefaultGitBinaries = []string{"/usr/bin/git"}

// GitHubTokens mints the read-only tokens of a code-host connection. The name is historical:
// the interface is provider-neutral (internal/codehost).
type GitHubTokens interface {
	ReadToken(ctx context.Context, conn store.Connection, scope codehost.Scope) (codehost.Token, error)
}

// GitHubApps mints tokens as the connection's App, with the private key read from the
// connection's stored secret fields. Client caches tokens until they near expiry. Hosts
// defaults to the shipped providers with Client as the GitHub client.
type GitHubApps struct {
	Keys   connections.Opener
	Client *github.Client
	Hosts  *codehost.Registry
}

// ReadToken implements GitHubTokens.
func (g GitHubApps) ReadToken(ctx context.Context, conn store.Connection, scope codehost.Scope) (codehost.Token, error) {
	hosts := g.Hosts
	if hosts == nil {
		hosts = providers.New(g.Client)
	}
	p, ok := hosts.Get(conn.Connector)
	if !ok {
		return codehost.Token{}, fmt.Errorf("connection %s is a %s connection, which is not a code host", conn.ID.String(), conn.Connector)
	}
	c, err := connections.Load(ctx, g.Keys, conn)
	if err != nil {
		return codehost.Token{}, err
	}
	acc, err := p.Open(ctx, c, scope)
	if err != nil {
		return codehost.Token{}, err
	}
	return acc.Token(), nil
}

// GitHubRead is what an agent's GitHub grant renders into its sandbox.
type GitHubRead struct {
	// Provider is how messages and the provider profile name the code host: "GitHub", "Gitea".
	Provider string
	// ConnectionID is the bound connection.
	ConnectionID string
	// Repositories are the bound repositories, owner/name.
	Repositories []string
	// Permissions the token is narrowed to; all "read".
	Permissions map[string]string
	// Endpoints are the enforced L7 endpoints the grant allows.
	Endpoints []execution.Endpoint
}

// GitHubReadAccess returns the direct GitHub access cfg grants. ok is false when the grant has
// no allowed scope with network rules, or no repositories are bound: then nothing is rendered
// and the sandbox cannot reach GitHub at all.
func GitHubReadAccess(cfg agentconfig.Config) (read GitHubRead, ok bool, err error) {
	return GitHubReadAccessAt(cfg, "")
}

// GitHubReadAccessAt is GitHubReadAccess for a connection whose host has the given base URL (the
// connection's baseUrl; empty is the connector's default host). The network rules are rendered
// at that host (ADR 0062), so nothing else is reachable.
func GitHubReadAccessAt(cfg agentconfig.Config, baseURL string) (read GitHubRead, ok bool, err error) {
	connector, g, found := providers.Default().Grant(cfg)
	if !found {
		return GitHubRead{}, false, nil
	}
	ct, found := connectors.Get(connector)
	if !found {
		return GitHubRead{}, false, fmt.Errorf("the %s connector type is not embedded", connector)
	}
	var scopes []string
	for _, s := range g.Scopes {
		if ct.HasNetwork(g.Preset, []string{s}) {
			scopes = append(scopes, s)
		}
	}
	b, bound := cfg.Binding(connector)
	if len(scopes) == 0 || !bound || len(b.Targets) == 0 {
		return GitHubRead{}, false, nil
	}
	loc, err := ct.Locate(baseURL)
	if err != nil {
		return GitHubRead{}, false, fmt.Errorf("github grant: %w", err)
	}
	hosts, err := ct.NetworkAt(loc, g.Preset, scopes, b.Targets)
	if err != nil {
		return GitHubRead{}, false, fmt.Errorf("github grant: %w", err)
	}
	perms := ct.Permissions(scopes...)
	for p, level := range perms {
		if level != "read" {
			return GitHubRead{}, false, fmt.Errorf("github grant: scope with network rules asks for %s=%s; tokens in the sandbox are read-only", p, level)
		}
	}
	name := connector
	if c, err := codehost.CapabilitiesOf(ct); err == nil && c.DisplayName != "" {
		name = c.DisplayName
	}
	read = GitHubRead{Provider: name, ConnectionID: b.ConnectionID, Repositories: slices.Clone(b.Targets), Permissions: perms}
	for _, h := range hosts {
		read.Endpoints = append(read.Endpoints, execution.Endpoint{
			Host: h.Host, Port: uint32(h.Port), //nolint:gosec // ports are schema-bounded
			Protocol: "rest", Allow: l7Rules(h.Allow), Deny: l7Rules(h.Deny),
		})
	}
	return read, len(read.Endpoints) > 0, nil
}

func l7Rules(in []connectors.HTTPRule) []execution.L7Rule {
	out := make([]execution.L7Rule, 0, len(in))
	for _, r := range in {
		out = append(out, execution.L7Rule{Method: r.Method, Path: r.Path, Query: r.Query})
	}
	return out
}

// GitHubProfile is the per-agent profile of the GitHub provider record: the token, and the
// endpoints and binaries it may be used with.
func GitHubProfile(id string, read GitHubRead, binaries []string) execution.ProviderProfile {
	name := cmp.Or(read.Provider, "GitHub")
	return execution.ProviderProfile{
		ID:          id,
		DisplayName: name + " (read)",
		Description: "Read-only " + name + " credential of one agent, rendered by koinon-reconciler",
		Category:    execution.CategorySourceControl,
		Credentials: []execution.ProfileCredential{{
			Name:       githubCredential,
			EnvVars:    slices.Clone(GitHubTokenEnvVars),
			Required:   true,
			AuthStyle:  "bearer",
			HeaderName: "authorization",
		}},
		Endpoints: slices.Clone(read.Endpoints),
		Binaries:  slices.Clone(binaries),
	}
}

// GitHubRule is the sandbox policy entry for read: the same endpoints as the profile.
func GitHubRule(read GitHubRead, binaries []string) execution.NetworkRule {
	return execution.NetworkRule{Endpoints: slices.Clone(read.Endpoints), Binaries: slices.Clone(binaries)}
}

// baseURLOf returns the base URL of the host of the agent's code-host connection, empty for the
// default host or an agent with no code-host binding.
func (r *Reconciler) baseURLOf(ctx context.Context, w *work) (string, error) {
	connector, b, ok := providers.Default().Binding(w.cfg)
	if !ok || b.ConnectionID == "" {
		return "", nil
	}
	var id pgtype.UUID
	if err := id.Scan(b.ConnectionID); err != nil || !id.Valid {
		return "", fmt.Errorf("github grant: bad connection ID %q", b.ConnectionID)
	}
	conn, err := r.q.GetConnection(ctx, store.GetConnectionParams{OrgID: w.org.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // ensureGitHub reports a missing connection when the grant needs one
	}
	if err != nil {
		return "", fmt.Errorf("github grant: read %s connection %s: %w", connector, b.ConnectionID, err)
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return "", fmt.Errorf("github grant: connection %s: %w", b.ConnectionID, err)
	}
	if site.BaseURL != "" {
		// Checked again at render with a fresh lookup, as for provider URLs.
		if err := r.cfg.URLPolicy.CheckStrict(ctx, site.BaseURL); err != nil {
			return "", fmt.Errorf("github grant: connection %s: %w", b.ConnectionID, err)
		}
	}
	return site.BaseURL, nil
}

// githubBinaries are the executables that may reach GitHub: git and the flavor's binaries.
func (r *Reconciler) githubBinaries(f Flavor) []string {
	out := slices.Clone(r.cfg.GitBinaries)
	for _, b := range f.Binaries {
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// ensureGitHub mints (or reuses) the agent's token and ensures its profile and provider record.
func (r *Reconciler) ensureGitHub(ctx context.Context, ws string, w *work, names execution.AgentNames, f Flavor, read GitHubRead) error {
	if r.cfg.GitHub == nil {
		return errors.New("the agent has a GitHub read grant, but no GitHub token minter is configured")
	}
	var connID pgtype.UUID
	if err := connID.Scan(read.ConnectionID); err != nil || !connID.Valid {
		return fmt.Errorf("github grant: bad connection ID %q", read.ConnectionID)
	}
	conn, err := r.q.GetConnection(ctx, store.GetConnectionParams{OrgID: w.org.ID, ID: connID})
	if err != nil {
		return fmt.Errorf("github grant: read connection %s: %w", read.ConnectionID, err)
	}
	tok, err := r.cfg.GitHub.ReadToken(ctx, conn, codehost.Scope{
		Repositories: read.Repositories, Permissions: read.Permissions,
	})
	if err != nil {
		return fmt.Errorf("github grant: mint installation token: %w", err)
	}
	// The revision names the token by its expiry, never by its value: a new token is pushed
	// into the existing record (OpenShell merges credentials; the stable placeholder keeps
	// resolving), so running processes switch to it without a restart.
	rev := "github-token-expires-" + tok.ExpiresAt.UTC().Format(time.RFC3339)
	if tok.Revision != "" {
		// A static token (Gitea) does not expire: its revision changes when the token does.
		rev = "github-token-" + tok.Revision
	}
	value := tok.Value()
	creds := make(map[string]string, len(GitHubTokenEnvVars))
	for _, env := range GitHubTokenEnvVars {
		creds[env] = value
	}
	if err := r.backend.EnsureProvider(ctx, ws, execution.Provider{
		Name:               names.GitHubProvider,
		Profile:            GitHubProfile(names.GitHubProfile, read, r.githubBinaries(f)),
		Credentials:        creds,
		CredentialRevision: rev,
		Labels:             ownerLabels(w.org.ID, w.agent.ID),
	}); err != nil {
		return err
	}
	if r.noteGitHubRevision(names.GitHubProvider, rev) {
		what := "installation token"
		if tok.Revision != "" {
			what = "read token"
		}
		r.log.Info("pushed "+cmp.Or(read.Provider, "GitHub")+" "+what, append(w.logAttrs(), "token", tok)...)
	}
	return nil
}

// noteGitHubRevision records the revision last pushed for a provider and reports whether it
// changed, so a refresh is logged once.
func (r *Reconciler) noteGitHubRevision(provider, rev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.githubRevs[provider] == rev {
		return false
	}
	r.githubRevs[provider] = rev
	return true
}

// deleteGitHub removes an agent's GitHub provider record and profile, for agents without (or no
// longer with) a read grant whose sandbox no longer references the record.
func (r *Reconciler) deleteGitHub(ctx context.Context, ws string, names execution.AgentNames) error {
	if err := r.backend.DeleteProvider(ctx, ws, names.GitHubProvider); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.githubRevs, names.GitHubProvider)
	r.mu.Unlock()
	return r.backend.DeleteProviderProfile(ctx, ws, names.GitHubProfile)
}
