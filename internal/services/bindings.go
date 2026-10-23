// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/templates"
)

// bindGrants validates the caller's grant bindings against the template and the org's
// connections and returns them for the agent config (the repository binding of plan item C2):
//
//   - each names a connector the template grants, once, and a connection of that type in the
//     org;
//   - its repositories are valid targets, distinct (GitHub names are case-insensitive), and as
//     many as the template grant's targets min and max allow (1 to 500 without bounds);
//   - the connection's installation can access every one of them: Koinon mints a metadata-read
//     token narrowed to them, which GitHub refuses for a repository the installation cannot see
//     and which Koinon checks covers exactly those full names (so the owner must be the
//     installation's account);
//   - every grant whose template sets a positive targets min is bound.
func (s *Server) bindGrants(ctx context.Context, org pgtype.UUID, t *templates.Template, in []*agentv1alpha1.GrantBinding) ([]agentconfig.Binding, error) {
	invalid := func(format string, args ...any) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(format, args...))
	}
	var out []agentconfig.Binding
	bound := map[string]bool{}
	for i, b := range in {
		field := fmt.Sprintf("grants[%d]", i)
		gi := slices.IndexFunc(t.Spec.Grants, func(g templates.Grant) bool { return g.Connector == b.GetConnector() })
		if gi < 0 {
			return nil, invalid("%s: the template has no grant on connector %q", field, b.GetConnector())
		}
		if bound[b.GetConnector()] {
			return nil, invalid("%s: the grant on %q is bound twice", field, b.GetConnector())
		}
		bound[b.GetConnector()] = true
		g := t.Spec.Grants[gi]
		ct, ok := connectors.Get(g.Connector)
		if !ok {
			return nil, invalid("%s: unknown connector type %q", field, g.Connector)
		}

		connID, err := parseID(field+".connection_id", b.GetConnectionId())
		if err != nil {
			return nil, err
		}
		conn, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: connID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, invalid("%s: connection not found", field)
		}
		if err != nil {
			return nil, s.dbError(err, "connection")
		}
		if conn.Connector != g.Connector {
			return nil, invalid("%s: connection %s is a %s connection, not %s", field, b.GetConnectionId(), conn.Connector, g.Connector)
		}

		repos := make([]string, 0, len(b.GetRepositories()))
		seen := map[string]bool{}
		for j, r := range b.GetRepositories() {
			r = strings.TrimSpace(r)
			if _, _, err := ct.SplitTarget(r); err != nil {
				return nil, invalid("%s.repositories[%d]: %q is not a repository (owner/name)", field, j, r)
			}
			if seen[strings.ToLower(r)] {
				return nil, invalid("%s.repositories[%d]: %q is listed twice", field, j, r)
			}
			seen[strings.ToLower(r)] = true
			repos = append(repos, r)
		}
		lo, hi := 1, github.MaxRepositories
		if g.Targets != nil {
			lo, hi = g.Targets.Min, g.Targets.Max
		}
		if len(repos) < lo || len(repos) > hi {
			return nil, invalid("%s: the template allows %d to %d repositories, not %d", field, lo, hi, len(repos))
		}
		if g.Connector == connectors.GitHubID {
			if repos, err = s.checkInstallation(ctx, conn, field, repos); err != nil {
				return nil, err
			}
		}
		out = append(out, agentconfig.Binding{Connector: g.Connector, ConnectionID: idString(conn.ID), Targets: repos})
	}
	for _, g := range t.Spec.Grants {
		if g.Targets != nil && g.Targets.Min > 0 && !bound[g.Connector] {
			return nil, invalid("the grant on %q needs a connection and %d to %d repositories (grants)", g.Connector, g.Targets.Min, g.Targets.Max)
		}
	}
	return out, nil
}

// checkInstallation asks GitHub, as the connection's App, for a metadata-read token narrowed to
// repos, and with it reads each repository's full_name: GitHub matches names
// case-insensitively but OpenShell matches the read grant's paths case-sensitively, so the
// binding stores GitHub's spelling. It returns the repositories in that spelling, in the order
// given. Nothing is written to GitHub.
func (s *Server) checkInstallation(ctx context.Context, conn store.Connection, field string, repos []string) ([]string, error) {
	app, err := connections.LoadGitHubApp(ctx, s.keys, conn)
	if err != nil {
		if errors.Is(err, connections.ErrNoKeyring) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		// The messages name the Secret or say the key is malformed; never its content.
		return nil, connect.NewError(connect.CodeFailedPrecondition, field+": the connection cannot authenticate to GitHub: "+err.Error())
	}
	tok, err := s.github.InstallationToken(ctx, app, github.TokenRequest{
		Repositories: repos, Permissions: map[string]string{"metadata": "read"},
	})
	if err != nil {
		return nil, s.githubError(conn, field, repos, err)
	}
	canonical := make([]string, 0, len(repos))
	for j, r := range repos {
		repo, err := github.ParseRepo(r)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%s.repositories[%d]: %v", field, j, err))
		}
		info, err := s.github.GetRepository(ctx, tok, repo)
		if err != nil {
			return nil, s.githubError(conn, field, repos, err)
		}
		// A renamed or transferred repository answers under its new name (GitHub redirects);
		// binding the old one would grant a path that no longer exists.
		if !strings.EqualFold(info.FullName, r) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
				"%s.repositories[%d]: %q is now %q on GitHub; bind it by that name", field, j, r, info.FullName))
		}
		canonical = append(canonical, info.FullName)
	}
	return canonical, nil
}

// githubError maps an error from GitHub while checking a binding to the caller's error.
func (s *Server) githubError(conn store.Connection, field string, repos []string, err error) error {
	var apiErr *github.APIError
	switch {
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnprocessableEntity || apiErr.Status == http.StatusNotFound):
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
			"%s: the connection's installation cannot access every repository in %v; install the App on them first", field, repos))
	case errors.As(err, &apiErr):
		return connect.NewError(connect.CodeFailedPrecondition, field+": GitHub refused the connection: "+apiErr.Error())
	case errors.Is(err, github.ErrRepositoryMismatch):
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
			"%s: %v are not all repositories of the connection's installation account", field, repos))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "request canceled")
	default:
		s.log.Warn("github installation check failed", "connection", idString(conn.ID), "err", err)
		return connect.NewError(connect.CodeUnavailable, field+": could not reach GitHub to check the repositories; try again")
	}
}
