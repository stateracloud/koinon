// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// The ProviderService is the UI's face of the catalog service layer (ADR 0056): the same
// validation, upstream URL policy and audit as the admin API. Every call needs the admin role, so
// members do not see providers or models at all.

func providerProto(p catalog.Provider) *providerv1alpha1.Provider {
	return &providerv1alpha1.Provider{
		Id: p.ID, Name: p.Name, Type: p.Type, Url: p.URL, HasKey: p.HasKey, Source: p.Source,
		ManagedBySecret: p.ManagedBySecret, DisabledReason: p.DisabledReason, Removed: p.Removed,
		AnthropicWorkspaceId: p.AnthropicWorkspaceID, Formats: formatProtos(p.Formats), CredentialLostAt: optTime(p.CredentialLostAt), AdminDisabled: adminDisabledProto(p.AdminDisabled),
		HasCostKey: p.HasCostKey, CostScope: p.CostScope, CostKeyLostAt: optTime(p.CostKeyLostAt),
		CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt),
	}
}

func modelProto(m catalog.Model) *providerv1alpha1.Model {
	return &providerv1alpha1.Model{
		Id: m.ID, ProviderId: m.ProviderID, Name: m.Name, Guard: m.Guard, Source: m.Source,
		Format: m.Format, FormatOverridden: m.FormatOverridden,
		ManagedBySecret: m.ManagedBySecret, DisabledReason: m.DisabledReason,
		CreatedAt: timestamppb.New(m.CreatedAt), UpdatedAt: timestamppb.New(m.UpdatedAt),
	}
}

// ratesFromProto carries the deprecated rates field to the catalog, which refuses it.
func ratesFromProto(r *providerv1alpha1.Rates) *catalog.Rates {
	if r == nil {
		return nil
	}
	return &catalog.Rates{Input: r.GetInput(), Output: r.GetOutput(), CacheRead: r.GetCacheRead()}
}

// catalogError maps the catalog's errors; messages are safe to show.
func (s *Server) catalogError(err error) error {
	var inv *catalog.InvalidError
	switch {
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.Is(err, catalog.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "not found")
	case errors.Is(err, mcpservers.ErrToolChanged):
		return connect.NewError(connect.CodeAborted, err.Error())
	case errors.Is(err, catalog.ErrReadOnly):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, catalog.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, "an entry with this name already exists")
	case errors.Is(err, catalog.ErrNoKeys):
		return connect.NewError(connect.CodeUnavailable, err.Error())
	}
	return s.dbError(err, "provider")
}

// providerCaller authorizes an admin call and checks the catalog is wired.
func (s *Server) providerCaller(ctx context.Context, orgID string) (caller, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleAdmin)
	if err != nil {
		return caller{}, err
	}
	if s.catalog == nil {
		return caller{}, connect.NewError(connect.CodeUnimplemented, "model providers are not configured")
	}
	return c, nil
}

// ListProviders implements ProviderService. Admins and owners.
func (s *Server) ListProviders(ctx context.Context, req *providerv1alpha1.ListProvidersRequest) (*providerv1alpha1.ListProvidersResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := s.catalog.ListProviders(ctx, c.org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	rows, err := s.q.ListProviderEchoChecks(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "provider")
	}
	checks := map[pgtype.UUID]modelcheck.Result{}
	for _, r := range rows {
		checks[r.ProviderID] = modelcheck.FromRow(r)
	}
	res := &providerv1alpha1.ListProvidersResponse{}
	for _, p := range list {
		pp := providerProto(p)
		if id, err := parseID("provider_id", p.ID); err == nil {
			if r, ok := checks[id]; ok {
				pp.EchoCheck = echoProto(r)
			}
		}
		res.Providers = append(res.Providers, pp)
	}
	return res, nil
}

// CreateProvider implements ProviderService. Admins and owners.
func (s *Server) CreateProvider(ctx context.Context, req *providerv1alpha1.CreateProviderRequest) (*providerv1alpha1.CreateProviderResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	p, err := s.catalog.CreateProvider(requestCtx(ctx), c.org, c.auditActor(), c.actor(), catalog.ProviderInput{
		Name: req.GetName(), Type: req.GetType(), URL: req.GetUrl(), APIKey: req.GetApiKey(),
		AnthropicWorkspaceID: req.GetAnthropicWorkspaceId(),
		CostAPIKey:           req.GetCostApiKey(), CostScope: req.GetCostScope(),
	})
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.CreateProviderResponse{Provider: providerProto(p)}, nil
}

// UpdateProvider implements ProviderService. Admins and owners.
func (s *Server) UpdateProvider(ctx context.Context, req *providerv1alpha1.UpdateProviderRequest) (*providerv1alpha1.UpdateProviderResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	p, err := s.catalog.UpdateProvider(requestCtx(ctx), c.org, c.auditActor(), req.GetProviderId(), catalog.ProviderPatch{
		Type: req.Type, URL: req.Url, APIKey: req.ApiKey, AnthropicWorkspaceID: req.AnthropicWorkspaceId,
		CostAPIKey: req.CostApiKey, CostScope: req.CostScope,
	})
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.UpdateProviderResponse{Provider: providerProto(p)}, nil
}

// DeleteProvider implements ProviderService. Admins and owners.
func (s *Server) DeleteProvider(ctx context.Context, req *providerv1alpha1.DeleteProviderRequest) (*providerv1alpha1.DeleteProviderResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := s.catalog.DeleteProvider(requestCtx(ctx), c.org, c.auditActor(), req.GetProviderId()); err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.DeleteProviderResponse{}, nil
}

// ListModels implements ProviderService. Admins and owners.
func (s *Server) ListModels(ctx context.Context, req *providerv1alpha1.ListModelsRequest) (*providerv1alpha1.ListModelsResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := s.catalog.ListModels(ctx, c.org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	res := &providerv1alpha1.ListModelsResponse{}
	for _, m := range list {
		res.Models = append(res.Models, modelProto(m))
	}
	return res, nil
}

// CreateModel implements ProviderService. Admins and owners.
func (s *Server) CreateModel(ctx context.Context, req *providerv1alpha1.CreateModelRequest) (*providerv1alpha1.CreateModelResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	m, err := s.catalog.CreateModel(requestCtx(ctx), c.org, c.auditActor(), c.actor(), catalog.ModelInput{
		ProviderID: req.GetProviderId(), Name: req.GetName(), Guard: req.GetGuard(), Format: req.GetFormat(), Rates: ratesFromProto(req.GetRates()), //nolint:staticcheck // deprecated field, refused by the catalog
	})
	if err != nil {
		return nil, s.catalogError(err)
	}
	var warnings []string
	if pid, err := parseID("provider_id", req.GetProviderId()); err == nil {
		warnings = s.prices.ModelWarnings(requestCtx(ctx), c.org, pid, m.Name, modelformat.Format(m.Format))
	}
	return &providerv1alpha1.CreateModelResponse{Model: modelProto(m), Warnings: warnings}, nil
}

// UpdateModel implements ProviderService. Admins and owners.
func (s *Server) UpdateModel(ctx context.Context, req *providerv1alpha1.UpdateModelRequest) (*providerv1alpha1.UpdateModelResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	m, err := s.catalog.UpdateModel(requestCtx(ctx), c.org, c.auditActor(), req.GetModelId(), catalog.ModelPatch{
		Guard: req.Guard, Format: req.Format, Rates: ratesFromProto(req.GetRates()), ClearRates: req.GetClearRates(), //nolint:staticcheck // deprecated fields, refused by the catalog
	})
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.UpdateModelResponse{Model: modelProto(m)}, nil
}

// DeleteModel implements ProviderService. Admins and owners.
func (s *Server) DeleteModel(ctx context.Context, req *providerv1alpha1.DeleteModelRequest) (*providerv1alpha1.DeleteModelResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := s.catalog.DeleteModel(requestCtx(ctx), c.org, c.auditActor(), req.GetModelId()); err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.DeleteModelResponse{}, nil
}

// GetDefaultModel implements ProviderService. Admins and owners.
func (s *Server) GetDefaultModel(ctx context.Context, req *providerv1alpha1.GetDefaultModelRequest) (*providerv1alpha1.GetDefaultModelResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	m, err := s.agentModels.GetDefault(ctx, c.org)
	if err != nil {
		return nil, s.agentModelError(err)
	}
	served, err := s.agentModels.ListServed(ctx, c.org, time.Now())
	if err != nil {
		return nil, s.agentModelError(err)
	}
	res := &providerv1alpha1.GetDefaultModelResponse{Model: m}
	for _, x := range served {
		res.Available = append(res.Available, &providerv1alpha1.DefaultModelChoice{Name: x.Name, Provider: x.Provider})
	}
	return res, nil
}

// SetDefaultModel implements ProviderService. Admins and owners.
func (s *Server) SetDefaultModel(ctx context.Context, req *providerv1alpha1.SetDefaultModelRequest) (*providerv1alpha1.SetDefaultModelResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(req.GetModel())
	if err := s.agentModels.SetDefault(requestCtx(ctx), c.org, c.auditActor(), want, time.Now()); err != nil {
		return nil, s.agentModelError(err)
	}
	return &providerv1alpha1.SetDefaultModelResponse{Model: want}, nil
}

// CheckProviderEcho implements ProviderService. Admins and owners.
func (s *Server) CheckProviderEcho(ctx context.Context, req *providerv1alpha1.CheckProviderEchoRequest) (*providerv1alpha1.CheckProviderEchoResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if s.modelCheck == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "the echo check is not configured")
	}
	pid, err := parseID("provider_id", req.GetProviderId())
	if err != nil {
		return nil, err
	}
	r, err := s.modelCheck.Check(requestCtx(ctx), c.org, pid, req.GetModel(), c.auditActor(), c.actor())
	if err != nil {
		return nil, s.echoError(err)
	}
	return &providerv1alpha1.CheckProviderEchoResponse{EchoCheck: echoProto(r)}, nil
}

func (s *Server) echoError(err error) error {
	var inv *modelcheck.InvalidError
	var un *modelcheck.UnavailableError
	var busy *modelcheck.BusyError
	switch {
	case errors.As(err, &busy):
		return connect.NewError(connect.CodeFailedPrecondition, busy.Error())
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.As(err, &un):
		return connect.NewError(connect.CodeUnavailable, un.Msg)
	case errors.Is(err, modelcheck.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "provider not found")
	}
	return s.dbError(err, "provider")
}

var echoOps = map[modelcheck.Op]providerv1alpha1.EchoDiffSegment_Op{
	modelcheck.Equal:    providerv1alpha1.EchoDiffSegment_OP_EQUAL,
	modelcheck.Deleted:  providerv1alpha1.EchoDiffSegment_OP_DELETED,
	modelcheck.Inserted: providerv1alpha1.EchoDiffSegment_OP_INSERTED,
}

func echoProto(r modelcheck.Result) *providerv1alpha1.EchoCheck {
	out := &providerv1alpha1.EchoCheck{
		Model: r.Model, Unchanged: r.Unchanged, Sent: r.Sent, Received: r.Received, CheckedAt: timestamppb.New(r.CheckedAt),
	}
	for _, d := range r.Diff {
		out.Diff = append(out.Diff, &providerv1alpha1.EchoDiffSegment{Op: echoOps[d.Op], Text: d.Text})
	}
	return out
}

// ModelFormats implements ProviderService. Admins and owners.
func (s *Server) ModelFormats(ctx context.Context, req *providerv1alpha1.ModelFormatsRequest) (*providerv1alpha1.ModelFormatsResponse, error) {
	c, err := s.providerCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	def, err := s.catalog.ModelFormats(requestCtx(ctx), c.org, req.GetProviderId(), req.GetName())
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &providerv1alpha1.ModelFormatsResponse{DefaultFormat: def}, nil
}

// ProviderTypeInfo implements ProviderService. Admins and owners.
func (s *Server) ProviderTypeInfo(ctx context.Context, req *providerv1alpha1.ProviderTypeInfoRequest) (*providerv1alpha1.ProviderTypeInfoResponse, error) {
	if _, err := s.providerCaller(ctx, req.GetOrgId()); err != nil {
		return nil, err
	}
	info := catalog.ProviderTypeInfo(req.GetType())
	return &providerv1alpha1.ProviderTypeInfoResponse{Formats: formatProtos(info.Formats), TakesWorkspaceId: info.WorkspaceID, TakesCostKey: info.CostKey}, nil
}

// formatProtos names formats with the labels of the format table.
func formatProtos(ids []string) []*providerv1alpha1.ModelFormat {
	out := make([]*providerv1alpha1.ModelFormat, 0, len(ids))
	for _, id := range ids {
		out = append(out, &providerv1alpha1.ModelFormat{Id: id, Label: modelformat.Label(modelformat.Format(id))})
	}
	return out
}
