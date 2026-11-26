// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	orgv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/org/v1alpha1"
	platformv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1"
	"github.com/stateracloud/koinon/internal/money"
)

// The org reads of the UI v2 (ADR 0126 section 4, issue #1264): the platform console's org list and
// org read (PlatformService, platform admins only) and the members read (OrgService, any role in the
// org). They only read; create, update, archive and delete need a decision (ADR 0126 section 7).

const (
	defaultOrgPageSize    = 50
	maxOrgPageSize        = 200
	defaultMemberPageSize = 100
	maxMemberPageSize     = 500
)

var errBadPageToken = connect.NewError(connect.CodeInvalidArgument, "page_token is not a next_page_token of this call")

// pageSize clamps a requested page size to (0, maxSize], with def for 0 or less.
func pageSize(req int32, def, maxSize int) int {
	switch {
	case req <= 0:
		return def
	case int(req) > maxSize:
		return maxSize
	default:
		return int(req)
	}
}

// ListOrgs implements PlatformService.
func (s *Server) ListOrgs(ctx context.Context, req *platformv1alpha1.ListOrgsRequest) (*platformv1alpha1.ListOrgsResponse, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	size := pageSize(req.GetPageSize(), defaultOrgPageSize, maxOrgPageSize)
	after := ""
	if t := req.GetPageToken(); t != "" {
		raw, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil || len(raw) == 0 {
			return nil, errBadPageToken
		}
		after = string(raw)
	}
	since := budget.MonthStart(time.Now())
	rows, err := s.q.ListOrgSummaries(ctx, store.ListOrgSummariesParams{
		Since:           pgtype.Timestamptz{Time: since, Valid: true},
		IncludePersonal: req.GetIncludePersonal(),
		AfterSlug:       after,
		RowLimit:        int32(size + 1), //nolint:gosec // size is at most maxOrgPageSize.
	})
	if err != nil {
		return nil, s.dbError(err, "orgs")
	}
	res := &platformv1alpha1.ListOrgsResponse{MonthStart: timestamppb.New(since)}
	if len(rows) > size {
		rows = rows[:size]
		res.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(rows[size-1].Slug))
	}
	for _, r := range rows {
		o, err := orgSummaryToProto(r)
		if err != nil {
			return nil, s.budgetError(err)
		}
		res.Orgs = append(res.Orgs, o)
	}
	return res, nil
}

// GetOrg implements PlatformService. The platform admin need not be a member of the org.
func (s *Server) GetOrg(ctx context.Context, req *platformv1alpha1.GetOrgRequest) (*platformv1alpha1.GetOrgResponse, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	org, err := parseID("org_id", req.GetOrgId())
	if err != nil {
		return nil, err
	}
	since := budget.MonthStart(time.Now())
	rows, err := s.q.ListOrgSummaries(ctx, store.ListOrgSummariesParams{
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		OnlyOrgID: org,
		RowLimit:  1,
	})
	if err != nil {
		return nil, s.dbError(err, "org")
	}
	if len(rows) == 0 {
		return nil, s.dbError(pgx.ErrNoRows, "org")
	}
	o, err := orgSummaryToProto(rows[0])
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &platformv1alpha1.GetOrgResponse{Org: o, MonthStart: timestamppb.New(since)}, nil
}

func orgSummaryToProto(r store.ListOrgSummariesRow) (*platformv1alpha1.OrgSummary, error) {
	spent, err := money.ParseNumeric(r.Spent)
	if err != nil {
		return nil, err
	}
	return &platformv1alpha1.OrgSummary{
		OrgId:              r.OrgID.String(),
		Slug:               r.Slug,
		DisplayName:        r.DisplayName,
		Kind:               orgKindProto(r.Kind),
		MemberCount:        clampInt32(r.MemberCount),
		AgentCount:         clampInt32(r.AgentCount),
		SpentThisMonthUsd:  spent.String(),
		MonthlyCapUsd:      formatAmount(r.MonthlyCap),
		PlatformCeilingUsd: formatAmount(r.PlatformCeiling),
		CreatedAt:          timestamppb.New(r.CreatedAt.Time),
	}, nil
}

func orgKindProto(k store.OrgKind) orgv1alpha1.OrgKind {
	switch k {
	case store.OrgKindPersonal:
		return orgv1alpha1.OrgKind_ORG_KIND_PERSONAL
	case store.OrgKindTeam:
		return orgv1alpha1.OrgKind_ORG_KIND_TEAM
	}
	return orgv1alpha1.OrgKind_ORG_KIND_UNSPECIFIED
}

func clampInt32(n int64) int32 {
	if n > 1<<31-1 {
		return 1<<31 - 1
	}
	return int32(n) //nolint:gosec // clamped above; counts are never negative.
}

// memberCursor is the position after the last member of a page: the sort key of ListOrgMembers.
type memberCursor struct {
	Name string `json:"n"`
	ID   string `json:"i"`
}

// ListMembers implements OrgService. Every role in the org may call it; a platform admin may call it
// for any org (authorize).
func (s *Server) ListMembers(ctx context.Context, req *orgv1alpha1.ListMembersRequest) (*orgv1alpha1.ListMembersResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	if _, member := c.p.RoleIn(c.org); !member {
		// A platform admin passes authorize for any org ID; the org must still be live, as for GetOrg.
		if _, err := s.q.GetOrgSlug(ctx, c.org); err != nil {
			return nil, s.dbError(err, "org")
		}
	}
	size := pageSize(req.GetPageSize(), defaultMemberPageSize, maxMemberPageSize)
	afterID := pgtype.UUID{Valid: true} // the zero UUID sorts before every user ID
	afterName := ""
	if t := req.GetPageToken(); t != "" {
		var cur memberCursor
		raw, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil || json.Unmarshal(raw, &cur) != nil {
			return nil, errBadPageToken
		}
		if err := afterID.Scan(cur.ID); err != nil || !afterID.Valid {
			return nil, errBadPageToken
		}
		afterName = cur.Name
	}
	rows, err := s.q.ListOrgMembers(ctx, store.ListOrgMembersParams{
		OrgID:     c.org,
		AfterName: afterName,
		AfterID:   afterID,
		RowLimit:  int32(size + 1), //nolint:gosec // size is at most maxMemberPageSize.
	})
	if err != nil {
		return nil, s.dbError(err, "members")
	}
	res := &orgv1alpha1.ListMembersResponse{}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		raw, err := json.Marshal(memberCursor{Name: last.DisplayName, ID: last.UserID.String()})
		if err != nil {
			return nil, s.dbError(errors.Join(errors.New("encode page token"), err), "members")
		}
		res.NextPageToken = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, r := range rows {
		res.Members = append(res.Members, &orgv1alpha1.Member{
			UserId:      r.UserID.String(),
			DisplayName: r.DisplayName,
			Role:        auth.RoleToProto(r.Role),
		})
	}
	return res, nil
}
