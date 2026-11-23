// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/githublink"
	"github.com/stateracloud/koinon/internal/outward"
)

// The GitHub link RPCs (issue #1175): a member links their own GitHub login through a GitHub
// connection's App; admins see and remove every link of the org.

func (s *Server) linksOrUnimplemented() error {
	if s.githubLinks == nil {
		return connect.NewError(connect.CodeUnimplemented, "GitHub links are not available on this install")
	}
	return nil
}

// githubClientID is the GitHub connection's OAuth client ID, or "".
func githubClientID(config []byte) string {
	cfg, err := github.ParseConfig(config)
	if err != nil {
		return ""
	}
	return cfg.ClientID
}

func (s *Server) StartGitHubLink(ctx context.Context, req *connectionv1alpha1.StartGitHubLinkRequest) (*connectionv1alpha1.StartGitHubLinkResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	if err := s.linksOrUnimplemented(); err != nil {
		return nil, err
	}
	id, err := parseID("connection_id", req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	u, err := s.githubLinks.Start(requestCtx(ctx), c.org, c.actor(), id)
	switch {
	case err == nil:
		return &connectionv1alpha1.StartGitHubLinkResponse{AuthorizeUrl: u}, nil
	case errors.Is(err, githublink.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, "connection not found")
	case errors.Is(err, githublink.ErrNotGitHub):
		return nil, connect.NewError(connect.CodeInvalidArgument, "only a GitHub connection links GitHub logins")
	case errors.Is(err, githublink.ErrNoUserAuth):
		return nil, connect.NewError(connect.CodeFailedPrecondition, "this connection's App cannot sign users in: create the App with the guided setup, which stores its client ID and secret")
	case errors.Is(err, githublink.ErrNoPublicURL):
		return nil, connect.NewError(connect.CodeFailedPrecondition, "Koinon has no public URL, so GitHub cannot send you back; an operator sets it")
	case errors.Is(err, connections.ErrNoKeyring), errors.Is(err, connections.ErrRemoved),
		errors.Is(err, connections.ErrCredentialLost), errors.Is(err, outward.ErrDisabled):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return nil, s.dbError(err, "GitHub link")
}

func linkProto(l githublink.Link) *connectionv1alpha1.GitHubLink {
	return &connectionv1alpha1.GitHubLink{
		UserId: l.UserID.String(), Host: l.Host, GithubLogin: l.GitHubLogin, GithubUserId: l.GitHubUserID,
		CreatedAt: timestamppb.New(l.CreatedAt), DisplayName: l.DisplayName, Email: l.Email,
	}
}

func (s *Server) ListMyGitHubLinks(ctx context.Context, req *connectionv1alpha1.ListMyGitHubLinksRequest) (*connectionv1alpha1.ListMyGitHubLinksResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	if err := s.linksOrUnimplemented(); err != nil {
		return nil, err
	}
	links, err := s.githubLinks.Mine(ctx, c.org, c.actor())
	if err != nil {
		return nil, s.dbError(err, "GitHub links")
	}
	out := &connectionv1alpha1.ListMyGitHubLinksResponse{}
	for _, l := range links {
		out.Links = append(out.Links, linkProto(l))
	}
	return out, nil
}

func (s *Server) ListGitHubLinks(ctx context.Context, req *connectionv1alpha1.ListGitHubLinksRequest) (*connectionv1alpha1.ListGitHubLinksResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.linksOrUnimplemented(); err != nil {
		return nil, err
	}
	links, err := s.githubLinks.List(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "GitHub links")
	}
	out := &connectionv1alpha1.ListGitHubLinksResponse{}
	for _, l := range links {
		out.Links = append(out.Links, linkProto(l))
	}
	return out, nil
}

func (s *Server) DeleteGitHubLink(ctx context.Context, req *connectionv1alpha1.DeleteGitHubLinkRequest) (*connectionv1alpha1.DeleteGitHubLinkResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	if err := s.linksOrUnimplemented(); err != nil {
		return nil, err
	}
	if req.GetHost() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "host is required")
	}
	user := c.actor()
	if req.GetUserId() != "" {
		if user, err = parseID("user_id", req.GetUserId()); err != nil {
			return nil, err
		}
	}
	if user != c.actor() && !c.p.HasRole(c.org, store.OrgRoleAdmin) {
		return nil, connect.NewError(connect.CodePermissionDenied, "only an admin removes another user's GitHub link")
	}
	err = s.githubLinks.Delete(requestCtx(ctx), c.org, user, req.GetHost(), c.auditActor())
	switch {
	case err == nil:
		return &connectionv1alpha1.DeleteGitHubLinkResponse{}, nil
	case errors.Is(err, githublink.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, "GitHub link not found")
	}
	return nil, s.dbError(err, "GitHub link")
}
