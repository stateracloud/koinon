// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/prices"
)

// The platform price list RPCs of PriceService (ADR 0098 slice 3). They are for a platform admin
// only. Writes call the Refresher and Service methods `koinon-api prices` calls, with the caller as
// the actor, so the install audit events (price.proposal.accepted, price.proposal.dismissed,
// platform_price.set, platform_price.rollback) carry the same fields as the commands'.

// BindPriceRefresher gives the platform price RPCs the refresher koinon-api runs, which they accept,
// dismiss and fetch through. Without it they answer UNIMPLEMENTED.
func (s *Server) BindPriceRefresher(r *prices.Refresher) { s.refresher = r }

// platformPriceCaller authorizes a platform admin and returns the actor to audit.
func (s *Server) platformPriceCaller(ctx context.Context) (audit.Actor, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return audit.Actor{}, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if !p.PlatformAdmin {
		return audit.Actor{}, withReason(connect.NewError(connect.CodePermissionDenied, "only a platform admin can do this"), "PLATFORM_ADMIN_REQUIRED")
	}
	return audit.User(p.UserID), nil
}

// platformPriceWriter is platformPriceCaller for the RPCs that need the refresher.
func (s *Server) platformPriceWriter(ctx context.Context) (audit.Actor, *prices.Refresher, error) {
	actor, err := s.platformPriceCaller(ctx)
	if err != nil {
		return audit.Actor{}, nil, err
	}
	if s.refresher == nil {
		return audit.Actor{}, nil, connect.NewError(connect.CodeUnimplemented, "the price refresh is not configured")
	}
	return actor, s.refresher, nil
}

func tsOrNil(t interface{ IsZero() bool }, ts *timestamppb.Timestamp) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return ts
}

func provenanceProto(p prices.Provenance) *budgetv1alpha1.PriceProvenance {
	return &budgetv1alpha1.PriceProvenance{
		Source: p.Source, Url: p.URL, FetchedAt: tsOrNil(p.FetchedAt, timestamppb.New(p.FetchedAt)), Sha256: p.SHA256, CommitSha: p.CommitSHA,
		CommittedAt: tsOrNil(p.CommittedAt, timestamppb.New(p.CommittedAt)), Etag: p.ETag, Licence: p.Licence, AgreeingSources: p.AgreeingSources,
		ProposalId: p.ProposalID, Approver: p.Approver, RuleSetVersion: p.RuleSetVersion,
		ApprovedAt: tsOrNil(p.ApprovedAt, timestamppb.New(p.ApprovedAt)), VouchedBy: p.VouchedBy,
	}
}

func proposalProto(p prices.Proposal) *budgetv1alpha1.PriceProposal {
	out := &budgetv1alpha1.PriceProposal{
		Id: p.ID, Source: p.Source, Origin: p.Origin, ProviderType: p.ProviderType, Model: p.Model, CurrentRef: p.CurrentRef,
		Provenance: provenanceProto(p.Provenance), AgreeingSources: p.Agreeing, Class: string(p.Class), FailedRules: p.FailedRules,
		State: p.State, CreatedAt: timestamppb.New(p.CreatedAt), DecidedBy: p.DecidedBy, AppliedVersion: p.Applied,
	}
	if p.Current != nil {
		out.Current = entryProto(*p.Current)
	}
	if p.Proposed != nil {
		out.Proposed = entryProto(*p.Proposed)
	}
	for _, c := range p.Changes {
		out.Changes = append(out.Changes, &budgetv1alpha1.PriceRateChange{Bucket: c.Bucket, ContextOver: c.ContextOver, From: c.From, To: c.To})
	}
	if p.DecidedAt != nil {
		out.DecidedAt = timestamppb.New(*p.DecidedAt)
	}
	return out
}

func platformVersionProto(v prices.PlatformVersion, withEntries bool) *budgetv1alpha1.PlatformPriceVersion {
	out := &budgetv1alpha1.PlatformPriceVersion{
		Version: v.Version, CreatedAt: timestamppb.New(v.CreatedAt), ValidFrom: timestamppb.New(v.ValidFrom), Actor: v.Actor, Reason: v.Reason,
		RuleSetVersion: v.RuleSetVersion, ProposalIds: v.ProposalIDs, RestoredFrom: v.RestoredFrom,
		BadEntries: int32(v.BadEntries), //nolint:gosec // a count of entries in one version
	}
	if withEntries {
		for _, e := range v.Entries {
			out.Entries = append(out.Entries, &budgetv1alpha1.PlatformPriceEntry{
				Entry: entryProto(e.Entry), PriceRef: prices.PlatformRef(v.Version, e.Entry.ID), Provenance: provenanceProto(e.Provenance),
			})
		}
	}
	return out
}

// ListPriceProposals implements PriceService. Platform admins.
func (s *Server) ListPriceProposals(ctx context.Context, req *budgetv1alpha1.ListPriceProposalsRequest) (*budgetv1alpha1.ListPriceProposalsResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = 100
	}
	list, err := s.prices.ListProposals(ctx, req.GetState(), req.GetClass(), limit)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.ListPriceProposalsResponse{}
	for _, p := range list {
		res.Proposals = append(res.Proposals, proposalProto(p))
	}
	return res, nil
}

// GetPriceProposal implements PriceService. Platform admins.
func (s *Server) GetPriceProposal(ctx context.Context, req *budgetv1alpha1.GetPriceProposalRequest) (*budgetv1alpha1.GetPriceProposalResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	p, err := s.prices.GetProposal(ctx, req.GetId())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.GetPriceProposalResponse{Proposal: proposalProto(p)}, nil
}

// AcceptPriceProposal implements PriceService. Platform admins.
func (s *Server) AcceptPriceProposal(ctx context.Context, req *budgetv1alpha1.AcceptPriceProposalRequest) (*budgetv1alpha1.AcceptPriceProposalResponse, error) {
	actor, r, err := s.platformPriceWriter(ctx)
	if err != nil {
		return nil, err
	}
	res, err := r.Accept(ctx, actor, req.GetId(), prices.AcceptOptions{Confirm: req.GetConfirm(), MaxOutput: int(req.GetMaxOutput())})
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.AcceptPriceProposalResponse{Version: res.Version.Version, Warnings: res.Warnings}, nil
}

// DismissPriceProposal implements PriceService. Platform admins.
func (s *Server) DismissPriceProposal(ctx context.Context, req *budgetv1alpha1.DismissPriceProposalRequest) (*budgetv1alpha1.DismissPriceProposalResponse, error) {
	actor, r, err := s.platformPriceWriter(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.Dismiss(ctx, actor, req.GetId()); err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.DismissPriceProposalResponse{}, nil
}

// ListPriceSourceFetches implements PriceService. Platform admins.
func (s *Server) ListPriceSourceFetches(ctx context.Context, req *budgetv1alpha1.ListPriceSourceFetchesRequest) (*budgetv1alpha1.ListPriceSourceFetchesResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = 100
	}
	list, err := s.prices.SourceFetches(ctx, limit)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.ListPriceSourceFetchesResponse{}
	for _, f := range list {
		res.Fetches = append(res.Fetches, &budgetv1alpha1.PriceSourceFetch{
			Id: f.ID, Source: f.Source, Url: f.URL, FetchedAt: timestamppb.New(f.FetchedAt), Result: f.Result, Error: f.Error,
			Sha256: f.SHA256, Etag: f.ETag, CommitSha: f.CommitSHA, ModelCount: int32(f.ModelCount), //nolint:gosec // a count of models in one source
			Trigger: f.Trigger,
		})
	}
	return res, nil
}

// ListPlatformPriceVersions implements PriceService. Platform admins.
func (s *Server) ListPlatformPriceVersions(ctx context.Context, req *budgetv1alpha1.ListPlatformPriceVersionsRequest) (*budgetv1alpha1.ListPlatformPriceVersionsResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = 100
	}
	list, err := s.prices.PlatformVersions(ctx, limit)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.ListPlatformPriceVersionsResponse{}
	for _, v := range list {
		res.Versions = append(res.Versions, platformVersionProto(v, false))
	}
	return res, nil
}

// GetPlatformPriceVersion implements PriceService. Platform admins.
func (s *Server) GetPlatformPriceVersion(ctx context.Context, req *budgetv1alpha1.GetPlatformPriceVersionRequest) (*budgetv1alpha1.GetPlatformPriceVersionResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	v, err := s.prices.PlatformVersion(ctx, req.GetVersion())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.GetPlatformPriceVersionResponse{Version: platformVersionProto(v, true)}, nil
}

// DiffPlatformPriceVersions implements PriceService. Platform admins.
func (s *Server) DiffPlatformPriceVersions(ctx context.Context, req *budgetv1alpha1.DiffPlatformPriceVersionsRequest) (*budgetv1alpha1.DiffPlatformPriceVersionsResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	from, to, changes, err := s.prices.DiffPlatform(ctx, req.GetFromVersion(), req.GetToVersion())
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.DiffPlatformPriceVersionsResponse{FromVersion: from, ToVersion: to}
	for _, c := range changes {
		pc := &budgetv1alpha1.PriceChange{Kind: c.Kind, Id: c.ID, ValidFrom: c.ValidFrom, Fields: c.Fields}
		if c.Before != nil {
			pc.Before = entryProto(*c.Before)
		}
		if c.After != nil {
			pc.After = entryProto(*c.After)
		}
		res.Changes = append(res.Changes, pc)
	}
	return res, nil
}

// RollbackPlatformPrices implements PriceService. Platform admins.
func (s *Server) RollbackPlatformPrices(ctx context.Context, req *budgetv1alpha1.RollbackPlatformPricesRequest) (*budgetv1alpha1.RollbackPlatformPricesResponse, error) {
	actor, err := s.platformPriceCaller(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetVersion() < 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "version must be a version number")
	}
	nv, err := s.prices.Rollback(ctx, actor, req.GetVersion())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.RollbackPlatformPricesResponse{Version: nv.Version}, nil
}

// GetPriceRefreshSettings implements PriceService. Platform admins.
func (s *Server) GetPriceRefreshSettings(ctx context.Context, _ *budgetv1alpha1.GetPriceRefreshSettingsRequest) (*budgetv1alpha1.GetPriceRefreshSettingsResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	st, err := s.prices.Settings(ctx)
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.GetPriceRefreshSettingsResponse{
		Mode: string(st.Mode), Interval: st.Interval.String(), Recorded: st.Recorded,
		MaxChangePercent: int32(st.MaxChangePercent), MaxAutoPerDay: int32(st.MaxAutoPerDay), //nolint:gosec // validated bounds
		UpdatedAt: tsOrNil(st.UpdatedAt, timestamppb.New(st.UpdatedAt)),
	}, nil
}

// CheckPriceSources implements PriceService. Platform admins. It is `koinon-api prices fetch`: a
// manual run that stores proposals and applies none.
func (s *Server) CheckPriceSources(ctx context.Context, _ *budgetv1alpha1.CheckPriceSourcesRequest) (*budgetv1alpha1.CheckPriceSourcesResponse, error) {
	_, r, err := s.platformPriceWriter(ctx)
	if err != nil {
		return nil, err
	}
	run, err := r.Run(ctx, prices.TriggerCLI, false)
	// A source that failed is part of the answer (the CLI prints it too) and sits in the fetch log;
	// any other error is the server's.
	if err != nil && len(run.Failed) == 0 {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.CheckPriceSourcesResponse{
		Fetched: run.Fetched, Failed: run.Failed, Proposals: int32(run.Proposals), //nolint:gosec // a count of proposals in one run
	}, nil
}

// ExplainPlatformPrice implements PriceService. Platform admins.
func (s *Server) ExplainPlatformPrice(ctx context.Context, req *budgetv1alpha1.ExplainPlatformPriceRequest) (*budgetv1alpha1.ExplainPlatformPriceResponse, error) {
	if _, err := s.platformPriceCaller(ctx); err != nil {
		return nil, err
	}
	if req.GetProviderType() == "" || req.GetModel() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "provider_type and model are required")
	}
	res, err := s.prices.Explain(ctx, req.GetProviderType(), req.GetModel())
	if errors.Is(err, prices.ErrUnpriced) {
		return nil, connect.NewError(connect.CodeNotFound, "no price for this model")
	} else if err != nil {
		return nil, s.budgetError(err)
	}
	out := &budgetv1alpha1.ExplainPlatformPriceResponse{
		Layer: res.Source, PriceRef: res.Ref, Entry: entryProto(res.Entry), PlatformVersion: res.PlatformVersion, ListVersion: res.ListVersion,
	}
	if res.Source == prices.SourcePlatform {
		if v, err := s.prices.PlatformVersion(ctx, res.PlatformVersion); err == nil {
			for _, e := range v.Entries {
				if e.Entry.ID == res.Entry.ID {
					out.Provenance = provenanceProto(e.Provenance)
				}
			}
		}
	}
	return out, nil
}
