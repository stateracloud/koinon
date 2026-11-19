// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/db/store"
	remoteagentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/remoteagent/v1alpha1"
	"github.com/stateracloud/koinon/internal/remoteagents"
)

// The RemoteAgentService is the UI's face of the remote agent service layer (ADR 0123 decisions 9, 11 and
// 13): the same validation, URL policy and audit as the admin API. Every call needs the admin role. The
// card text is for admins only: GetRemoteAgentCard is the one call that returns it.

var remoteHealthToProto = map[string]string{
	remoteagents.HealthUnknown: "unknown", remoteagents.HealthHealthy: "healthy", remoteagents.HealthUnhealthy: "unhealthy",
}

func remoteAgentProto(org string, r remoteagents.RemoteAgent) *remoteagentv1alpha1.RemoteAgent {
	return &remoteagentv1alpha1.RemoteAgent{
		Id: r.ID, OrgId: org, Name: r.Name, CardUrl: r.CardURL, HasCredential: r.HasCredential,
		CredentialHeader: r.CredentialHeader, CredentialPrefix: r.CredentialPrefix, CredentialLostAt: optTime(r.CredentialLostAt),
		CaBundle: r.CABundle, InsecureSkipVerify: r.InsecureSkipVerify, CardHash: r.CardHash, CardCheckedAt: optTime(r.CardChecked),
		ObservedCardHash: r.ObservedCardHash, Health: remoteHealthToProto[r.Health], HealthReason: r.HealthReason,
		HealthCheckedAt: optTime(r.HealthCheckedAt), HealthSince: optTime(r.HealthSince), ConsecutiveFailures: int32(r.ConsecutiveFailures), //nolint:gosec // a count of failed checks
		DisabledReason: r.DisabledReason, CreatedAt: timestamppb.New(r.CreatedAt), UpdatedAt: timestamppb.New(r.UpdatedAt),
	}
}

// remoteAgentError maps the service's errors; messages are safe to show.
func (s *Server) remoteAgentError(err error) error {
	if errors.Is(err, remoteagents.ErrCardMoved) || errors.Is(err, remoteagents.ErrChanged) {
		return connect.NewError(connect.CodeAborted, err.Error())
	}
	return s.catalogError(err)
}

// remoteAgentCaller authorizes an admin call and checks the service is wired.
func (s *Server) remoteAgentCaller(ctx context.Context, orgID string) (caller, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleAdmin)
	if err != nil {
		return caller{}, err
	}
	if s.remoteAgents == nil {
		return caller{}, connect.NewError(connect.CodeUnimplemented, "remote agents are not configured")
	}
	return c, nil
}

// ListRemoteAgents implements RemoteAgentService. Admins and owners.
func (s *Server) ListRemoteAgents(ctx context.Context, req *remoteagentv1alpha1.ListRemoteAgentsRequest) (*remoteagentv1alpha1.ListRemoteAgentsResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := s.remoteAgents.List(ctx, c.org)
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	res := &remoteagentv1alpha1.ListRemoteAgentsResponse{}
	for _, r := range list {
		res.RemoteAgents = append(res.RemoteAgents, remoteAgentProto(req.GetOrgId(), r))
	}
	return res, nil
}

// CreateRemoteAgent implements RemoteAgentService. Admins and owners.
func (s *Server) CreateRemoteAgent(ctx context.Context, req *remoteagentv1alpha1.CreateRemoteAgentRequest) (*remoteagentv1alpha1.CreateRemoteAgentResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	r, err := s.remoteAgents.Create(requestCtx(ctx), c.org, c.auditActor(), c.actor(), remoteagents.Input{
		Name: req.GetName(), CardURL: req.GetCardUrl(), CredentialHeader: req.GetCredentialHeader(), CredentialPrefix: req.GetCredentialPrefix(),
		CredentialValue: req.GetCredentialValue(), CABundle: req.GetCaBundle(), InsecureSkipVerify: req.GetInsecureSkipVerify(),
	})
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.CreateRemoteAgentResponse{RemoteAgent: remoteAgentProto(req.GetOrgId(), r)}, nil
}

// UpdateRemoteAgent implements RemoteAgentService. Admins and owners.
func (s *Server) UpdateRemoteAgent(ctx context.Context, req *remoteagentv1alpha1.UpdateRemoteAgentRequest) (*remoteagentv1alpha1.UpdateRemoteAgentResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	patch := remoteagents.Patch{CardURL: req.CardUrl, CABundle: req.CaBundle, InsecureSkipVerify: req.InsecureSkipVerify, RemoveCredential: req.GetRemoveCredential()}
	if req.CredentialHeader != nil || req.CredentialPrefix != nil || req.CredentialValue != nil {
		patch.Credential = &remoteagents.CredentialPatch{Header: req.GetCredentialHeader(), Prefix: req.GetCredentialPrefix(), Value: req.CredentialValue}
	}
	r, err := s.remoteAgents.Update(requestCtx(ctx), c.org, c.auditActor(), req.GetRemoteAgentId(), patch)
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.UpdateRemoteAgentResponse{RemoteAgent: remoteAgentProto(req.GetOrgId(), r)}, nil
}

// DeleteRemoteAgent implements RemoteAgentService. Admins and owners.
func (s *Server) DeleteRemoteAgent(ctx context.Context, req *remoteagentv1alpha1.DeleteRemoteAgentRequest) (*remoteagentv1alpha1.DeleteRemoteAgentResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := s.remoteAgents.Delete(requestCtx(ctx), c.org, c.auditActor(), req.GetRemoteAgentId()); err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.DeleteRemoteAgentResponse{}, nil
}

// GetRemoteAgentCard implements RemoteAgentService. Admins and owners.
func (s *Server) GetRemoteAgentCard(ctx context.Context, req *remoteagentv1alpha1.GetRemoteAgentCardRequest) (*remoteagentv1alpha1.GetRemoteAgentCardResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	card, err := s.remoteAgents.GetCard(ctx, c.org, req.GetRemoteAgentId())
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.GetRemoteAgentCardResponse{
		CardHash: card.Hash, CardJson: card.JSON, ObservedCardHash: card.ObservedHash, ObservedCardJson: card.ObservedJSON,
	}, nil
}

// AcceptRemoteAgentCard implements RemoteAgentService. Admins and owners.
func (s *Server) AcceptRemoteAgentCard(ctx context.Context, req *remoteagentv1alpha1.AcceptRemoteAgentCardRequest) (*remoteagentv1alpha1.AcceptRemoteAgentCardResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	r, err := s.remoteAgents.AcceptCard(requestCtx(ctx), c.org, c.auditActor(), req.GetRemoteAgentId(), req.GetObservedCardHash())
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.AcceptRemoteAgentCardResponse{RemoteAgent: remoteAgentProto(req.GetOrgId(), r)}, nil
}

// GetAgentRemoteAgents implements RemoteAgentService. Admins and owners.
func (s *Server) GetAgentRemoteAgents(ctx context.Context, req *remoteagentv1alpha1.GetAgentRemoteAgentsRequest) (*remoteagentv1alpha1.GetAgentRemoteAgentsResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	agent, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	ids, err := s.remoteAgents.AgentGrants(ctx, c.org, agent)
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.GetAgentRemoteAgentsResponse{RemoteAgentIds: ids}, nil
}

// SetAgentRemoteAgents implements RemoteAgentService. Admins and owners.
func (s *Server) SetAgentRemoteAgents(ctx context.Context, req *remoteagentv1alpha1.SetAgentRemoteAgentsRequest) (*remoteagentv1alpha1.SetAgentRemoteAgentsResponse, error) {
	c, err := s.remoteAgentCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	agent, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	ids, err := s.remoteAgents.SetAgentGrants(requestCtx(ctx), c.org, c.auditActor(), agent, req.GetRemoteAgentIds())
	if err != nil {
		return nil, s.remoteAgentError(err)
	}
	return &remoteagentv1alpha1.SetAgentRemoteAgentsResponse{RemoteAgentIds: ids}, nil
}
