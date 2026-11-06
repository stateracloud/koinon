// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/orgsettings"
)

func (s *Server) runContentRetentionView(ctx context.Context, org pgtype.UUID) (RunContentRetention, error) {
	days, err := s.leakGuard.RunContentRetentionDays(ctx, org)
	if err != nil {
		return RunContentRetention{}, err
	}
	return RunContentRetention{
		Days: days, DefaultDays: orgsettings.DefaultRunContentRetentionDays,
		MinDays: orgsettings.MinRunContentRetentionDays, MaxDays: orgsettings.MaxRunContentRetentionDays,
	}, nil
}

// GetRunContentRetention implements StrictServerInterface.
func (s *Server) GetRunContentRetention(ctx context.Context, _ GetRunContentRetentionRequestObject) (GetRunContentRetentionResponseObject, error) {
	p, _ := s.actor(ctx)
	v, err := s.runContentRetentionView(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return GetRunContentRetentiondefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetRunContentRetention200JSONResponse(v), nil
}

// SetRunContentRetention implements StrictServerInterface.
func (s *Server) SetRunContentRetention(ctx context.Context, req SetRunContentRetentionRequestObject) (SetRunContentRetentionResponseObject, error) {
	p, actor := s.actor(ctx)
	// 0 means the default, as on the UI API.
	n := req.Body.Days
	if n != nil && *n == 0 {
		n = nil
	}
	err := s.leakGuard.SetRunContentRetention(ctx, p.Org, actor, n)
	if err == nil {
		var v RunContentRetention
		if v, err = s.runContentRetentionView(ctx, p.Org); err == nil {
			return SetRunContentRetention200JSONResponse(v), nil
		}
	}
	st, e := s.problem(err)
	return SetRunContentRetentiondefaultJSONResponse{Body: e, StatusCode: st}, nil
}
