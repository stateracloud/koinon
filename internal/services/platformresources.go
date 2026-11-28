// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/base64"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	platformv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1/platformv1alpha1connect"
)

// The platform console's lists of org resources (ADR 0126 section 4, issue #1412): the providers, the
// templates and the MCP servers of every org, for platform admins only. Each list reads org by org with
// the same code as the per-org list, so a row here is the row the org's own page shows.

const (
	defaultResourceOrgPageSize = 20
	maxResourceOrgPageSize     = 100
)

// platformResourcesAPI implements PlatformResourceService. It is its own type because the method
// names (ListProviders, ListTemplates) are also methods of Server for the per-org services.
type platformResourcesAPI struct{ s *Server }

var _ platformv1alpha1connect.PlatformResourceServiceHandler = platformResourcesAPI{}

// orgPage is the orgs of one page of a resource list.
type orgPage struct {
	orgs []store.ListOrgRefsRow
	// first is set on the first page of a list over every org.
	first bool
	next  string
}

// resourcePage is the part of each list request that selects the orgs.
type resourcePage interface {
	GetOrgId() string
	GetIncludePersonal() bool
	GetPageSize() int32
	GetPageToken() string
}

// orgPage authorizes a platform admin and reads the orgs that the page covers.
func (a platformResourcesAPI) orgPage(ctx context.Context, pg resourcePage) (orgPage, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return orgPage{}, err
	}
	var only pgtype.UUID
	if id := pg.GetOrgId(); id != "" {
		var err error
		if only, err = parseID("org_id", id); err != nil {
			return orgPage{}, err
		}
	}
	size := pageSize(pg.GetPageSize(), defaultResourceOrgPageSize, maxResourceOrgPageSize)
	after := ""
	if t := pg.GetPageToken(); t != "" {
		raw, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil || len(raw) == 0 {
			return orgPage{}, errBadPageToken
		}
		after = string(raw)
	}
	rows, err := a.s.q.ListOrgRefs(ctx, store.ListOrgRefsParams{
		OnlyOrgID:       only,
		IncludePersonal: pg.GetIncludePersonal(),
		AfterSlug:       after,
		RowLimit:        int32(size + 1), //nolint:gosec // size is at most maxResourceOrgPageSize.
	})
	if err != nil {
		return orgPage{}, a.s.dbError(err, "orgs")
	}
	if only.Valid && after == "" && len(rows) == 0 {
		return orgPage{}, a.s.dbError(pgx.ErrNoRows, "org")
	}
	p := orgPage{orgs: rows, first: !only.Valid && after == ""}
	if len(rows) > size {
		p.orgs = rows[:size]
		p.next = base64.RawURLEncoding.EncodeToString([]byte(rows[size-1].Slug))
	}
	return p, nil
}

func orgRefProto(r store.ListOrgRefsRow) *platformv1alpha1.OrgRef {
	return &platformv1alpha1.OrgRef{OrgId: r.OrgID.String(), Slug: r.Slug, DisplayName: r.DisplayName, Kind: orgKindProto(r.Kind)}
}

// ListProviders implements PlatformResourceService.
func (a platformResourcesAPI) ListProviders(ctx context.Context, req *platformv1alpha1.ListProvidersRequest) (*platformv1alpha1.ListProvidersResponse, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	if a.s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "model providers are not configured")
	}
	p, err := a.orgPage(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &platformv1alpha1.ListProvidersResponse{NextPageToken: p.next}
	for _, o := range p.orgs {
		list, err := a.s.orgProviders(ctx, o.OrgID)
		if err != nil {
			return nil, err
		}
		ref := orgRefProto(o)
		for _, pr := range list {
			res.Providers = append(res.Providers, &platformv1alpha1.OrgProvider{Org: ref, Provider: pr})
		}
	}
	return res, nil
}

// ListTemplates implements PlatformResourceService.
func (a platformResourcesAPI) ListTemplates(ctx context.Context, req *platformv1alpha1.ListTemplatesRequest) (*platformv1alpha1.ListTemplatesResponse, error) {
	p, err := a.orgPage(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &platformv1alpha1.ListTemplatesResponse{NextPageToken: p.next}
	if p.first {
		rows, err := a.s.q.ListCurrentTemplates(ctx)
		if err != nil {
			return nil, a.s.dbError(err, "templates")
		}
		for _, t := range a.s.templateProtos(rows) {
			res.Templates = append(res.Templates, &platformv1alpha1.OrgTemplate{Template: t})
		}
	}
	for _, o := range p.orgs {
		rows, err := a.s.q.ListOrgTemplates(ctx, o.OrgID)
		if err != nil {
			return nil, a.s.dbError(err, "templates")
		}
		ref := orgRefProto(o)
		for _, t := range a.s.templateProtos(rows) {
			res.Templates = append(res.Templates, &platformv1alpha1.OrgTemplate{Org: ref, Template: t})
		}
	}
	return res, nil
}

// ListMcpServers implements PlatformResourceService.
func (a platformResourcesAPI) ListMcpServers(ctx context.Context, req *platformv1alpha1.ListMcpServersRequest) (*platformv1alpha1.ListMcpServersResponse, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	if a.s.mcp == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "remote MCP servers are not configured")
	}
	p, err := a.orgPage(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &platformv1alpha1.ListMcpServersResponse{NextPageToken: p.next}
	for _, o := range p.orgs {
		list, err := a.s.orgMcpServers(ctx, o.OrgID, o.Slug)
		if err != nil {
			return nil, err
		}
		ref := orgRefProto(o)
		for _, m := range list {
			res.Servers = append(res.Servers, &platformv1alpha1.OrgMcpServer{Org: ref, Server: m})
		}
	}
	return res, nil
}
