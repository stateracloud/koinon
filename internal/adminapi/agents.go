// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/triggers"
)

// Agent settings, run-now and the provider echo check (rc.6). The service layers validate and
// audit; these handlers translate.

// TriggerRunner fires a schedule trigger now: *services.Server, whose RunScheduleTrigger is the
// path the scheduler's fire takes.
type TriggerRunner interface {
	RunScheduleTrigger(ctx context.Context, in triggers.RunNow) (store.Task, error)
}

// WithTriggers sets what fires a schedule trigger now. Without it run-now answers 503.
func (s *Server) WithTriggers(r TriggerRunner) *Server {
	s.triggers = r
	return s
}

// WithModelCheck sets the provider echo check. Without it the check answers 503.
func (s *Server) WithModelCheck(m *modelcheck.Service) *Server {
	s.modelCheck = m
	return s
}

func unavailable(msg string) (int, Error) {
	var e Error
	e.Error.Code, e.Error.Message = Internal, msg
	return http.StatusServiceUnavailable, e
}

func toAgentModel(v agentmodel.View) AgentModel {
	out := AgentModel{Setting: agentmodel.Default, Effective: v.Effective, Source: AgentModelSource(sourceName(v.Source))}
	if v.Setting != "" {
		out.Setting = v.Setting
	}
	if v.VersionModel != "" {
		out.VersionModel = &v.VersionModel
	}
	if v.Problem != "" {
		out.Problem = &v.Problem
	}
	out.Available = make([]struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
	}, 0, len(v.Available))
	for _, m := range v.Available {
		out.Available = append(out.Available, struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		}{Name: m.Name, Provider: m.Provider})
	}
	return out
}

func sourceName(s agentmodel.Source) string {
	if s == agentmodel.SourceNone {
		return "none"
	}
	return string(s)
}

// GetAgentModel implements StrictServerInterface.
func (s *Server) GetAgentModel(ctx context.Context, req GetAgentModelRequestObject) (GetAgentModelResponseObject, error) {
	p, _ := s.actor(ctx)
	_, v, err := s.agentModels.Load(ctx, p.Org, pgUUID(req.AgentId.String()), time.Now())
	if err != nil {
		st, e := s.problem(err)
		return GetAgentModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetAgentModel200JSONResponse(toAgentModel(v)), nil
}

// SetAgentModel implements StrictServerInterface.
func (s *Server) SetAgentModel(ctx context.Context, req SetAgentModelRequestObject) (SetAgentModelResponseObject, error) {
	p, actor := s.actor(ctx)
	agent := pgUUID(req.AgentId.String())
	if _, err := s.agentModels.Set(ctx, p.Org, agent, actor, req.Body.Model, time.Now()); err != nil {
		st, e := s.problem(err)
		return SetAgentModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	_, v, err := s.agentModels.Load(ctx, p.Org, agent, time.Now())
	if err != nil {
		st, e := s.problem(err)
		return SetAgentModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetAgentModel200JSONResponse(toAgentModel(v)), nil
}

// RunAgentTrigger implements StrictServerInterface.
func (s *Server) RunAgentTrigger(ctx context.Context, req RunAgentTriggerRequestObject) (RunAgentTriggerResponseObject, error) {
	p, actor := s.actor(ctx)
	if s.triggers == nil {
		st, e := unavailable("running a trigger now is not configured")
		return RunAgentTriggerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	task, err := s.triggers.RunScheduleTrigger(ctx, triggers.RunNow{
		Org: p.Org, Agent: pgUUID(req.AgentId.String()), Trigger: req.TriggerId, Actor: actor, User: p.CreatedBy,
	})
	if err != nil {
		st, e := s.problem(err)
		return RunAgentTriggerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return RunAgentTrigger202JSONResponse(TriggerRun{TaskId: openapi_types.UUID(task.ID.Bytes)}), nil
}

func toEchoCheck(r modelcheck.Result) EchoCheck {
	out := EchoCheck{Model: r.Model, Unchanged: r.Unchanged, Sent: r.Sent, Received: r.Received, CheckedAt: r.CheckedAt}
	out.Diff = make([]struct {
		Op   EchoCheckDiffOp `json:"op"`
		Text string          `json:"text"`
	}, 0, len(r.Diff))
	for _, d := range r.Diff {
		out.Diff = append(out.Diff, struct {
			Op   EchoCheckDiffOp `json:"op"`
			Text string          `json:"text"`
		}{Op: EchoCheckDiffOp(d.Op), Text: d.Text})
	}
	return out
}

// echoChecks loads the latest echo check of each of the org's providers.
func (s *Server) echoChecks(ctx context.Context, org pgtype.UUID) (map[pgtype.UUID]modelcheck.Result, error) {
	rows, err := store.New(s.pool).ListProviderEchoChecks(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make(map[pgtype.UUID]modelcheck.Result, len(rows))
	for _, r := range rows {
		out[r.ProviderID] = modelcheck.FromRow(r)
	}
	return out, nil
}

// CheckProviderEcho implements StrictServerInterface.
func (s *Server) CheckProviderEcho(ctx context.Context, req CheckProviderEchoRequestObject) (CheckProviderEchoResponseObject, error) {
	p, actor := s.actor(ctx)
	if s.modelCheck == nil {
		st, e := unavailable("the echo check is not configured")
		return CheckProviderEchodefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	r, err := s.modelCheck.Check(ctx, p.Org, pgUUID(req.ProviderId.String()), req.Body.Model, actor, p.CreatedBy)
	if err != nil {
		st, e := s.problem(err)
		return CheckProviderEchodefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return CheckProviderEcho200JSONResponse(toEchoCheck(r)), nil
}
