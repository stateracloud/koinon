// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
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
// What OpenShell 0.1.5 cannot express is listed in connectors/README.md.
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

// GitHubTokens mints installation tokens for a GitHub connection.
type GitHubTokens interface {
	InstallationToken(ctx context.Context, conn store.Connection, req github.TokenRequest) (github.Token, error)
}

// GitHubApps mints tokens as the connection's App, with the private key read from the
// connection's Secret. Client caches tokens until they near expiry.
type GitHubApps struct {
	Secrets connections.SecretStore
	Client  *github.Client
}

// InstallationToken implements GitHubTokens.
func (g GitHubApps) InstallationToken(ctx context.Context, conn store.Connection, req github.TokenRequest) (github.Token, error) {
	app, err := connections.LoadGitHubApp(ctx, g.Secrets, conn)
	if err != nil {
		return github.Token{}, err
	}
	return g.Client.InstallationToken(ctx, app, req)
}

// GitHubRead is what an agent's GitHub grant renders into its sandbox.
type GitHubRead struct {
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
	g, found := cfg.Grant(connectors.GitHubID)
	if !found {
		return GitHubRead{}, false, nil
	}
	ct, found := connectors.Get(connectors.GitHubID)
	if !found {
		return GitHubRead{}, false, errors.New("the GitHub connector type is not embedded")
	}
	var scopes []string
	for _, s := range g.Scopes {
		if ct.HasNetwork(g.Preset, []string{s}) {
			scopes = append(scopes, s)
		}
	}
	b, bound := cfg.Binding(connectors.GitHubID)
	if len(scopes) == 0 || !bound || len(b.Targets) == 0 {
		return GitHubRead{}, false, nil
	}
	hosts, err := ct.Network(g.Preset, scopes, b.Targets)
	if err != nil {
		return GitHubRead{}, false, fmt.Errorf("github grant: %w", err)
	}
	perms := ct.Permissions(scopes...)
	for p, level := range perms {
		if level != "read" {
			return GitHubRead{}, false, fmt.Errorf("github grant: scope with network rules asks for %s=%s; tokens in the sandbox are read-only", p, level)
		}
	}
	read = GitHubRead{ConnectionID: b.ConnectionID, Repositories: slices.Clone(b.Targets), Permissions: perms}
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
	return execution.ProviderProfile{
		ID:          id,
		DisplayName: "GitHub (read)",
		Description: "Read-only installation token of one agent, rendered by koinon-reconciler",
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
	tok, err := r.cfg.GitHub.InstallationToken(ctx, conn, github.TokenRequest{
		Repositories: read.Repositories, Permissions: read.Permissions,
	})
	if err != nil {
		return fmt.Errorf("github grant: mint installation token: %w", err)
	}
	// The revision names the token by its expiry, never by its value: a new token is pushed
	// into the existing record (OpenShell merges credentials; the stable placeholder keeps
	// resolving), so running processes switch to it without a restart.
	rev := "github-token-expires-" + tok.ExpiresAt.UTC().Format(time.RFC3339)
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
		r.log.Info("pushed GitHub installation token", append(w.logAttrs(), "token", tok)...)
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
