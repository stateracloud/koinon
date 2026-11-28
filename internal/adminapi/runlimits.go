// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The org's run limits (issue #492): the REST face of orgsettings.Service.RunLimits and
// SetRunLimits, with the same validation and audit (org.run_limits.set) as the UI API.

func runLimitJSON(l orgsettings.Limit) RunLimit {
	return RunLimit{Value: l.Value, DefaultValue: l.Default, Min: l.Min, Max: l.Max}
}

func (s *Server) runLimitsView(ctx context.Context, org pgtype.UUID) (RunLimits, error) {
	l, err := s.leakGuard.RunLimits(ctx, org)
	if err != nil {
		return RunLimits{}, err
	}
	v := l.View()
	return RunLimits{
		MaxConcurrentRuns: runLimitJSON(v.MaxConcurrentRuns), TriggerConcurrency: runLimitJSON(v.TriggerConcurrency),
		TriggerRatePerMinute: runLimitJSON(v.TriggerRatePerMinute), SandboxCreationsPerMinute: runLimitJSON(v.SandboxCreationsPerMinute),
	}, nil
}

// GetRunLimits implements StrictServerInterface.
func (s *Server) GetRunLimits(ctx context.Context, _ GetRunLimitsRequestObject) (GetRunLimitsResponseObject, error) {
	p, _ := s.actor(ctx)
	v, err := s.runLimitsView(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return GetRunLimitsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetRunLimits200JSONResponse(v), nil
}

// SetRunLimits implements StrictServerInterface. A limit left out stays; 0 returns it to the default.
func (s *Server) SetRunLimits(ctx context.Context, req SetRunLimitsRequestObject) (SetRunLimitsResponseObject, error) {
	p, actor := s.actor(ctx)
	b := req.Body
	err := s.leakGuard.SetRunLimits(ctx, p.Org, actor, orgsettings.RunLimitsPatch{
		MaxConcurrentRuns: b.MaxConcurrentRuns, TriggerConcurrency: b.TriggerConcurrency,
		TriggerRatePerMinute: b.TriggerRatePerMinute, SandboxCreationsPerMinute: b.SandboxCreationsPerMinute,
	})
	if err == nil {
		var v RunLimits
		if v, err = s.runLimitsView(ctx, p.Org); err == nil {
			return SetRunLimits200JSONResponse(v), nil
		}
	}
	st, e := s.problem(err)
	return SetRunLimitsdefaultJSONResponse{Body: e, StatusCode: st}, nil
}
