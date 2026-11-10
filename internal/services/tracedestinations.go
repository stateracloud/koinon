// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	tracev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1/tracev1alpha1connect"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
)

// traceAPI is the TraceDestinationService (ADR 0074). The org calls need the admin role; only an owner
// sets the content level full. The platform calls need a platform admin. The checks that make the
// rules hold (the owner for full, the ceiling, the test before saving) live in package tracedest, so
// any other face of the service layer gets them too.
type traceAPI struct{ s *Server }

var _ tracev1alpha1connect.TraceDestinationServiceHandler = traceAPI{}

var backendToProto = map[string]tracev1alpha1.Backend{
	string(export.Langfuse):  tracev1alpha1.Backend_BACKEND_LANGFUSE,
	string(export.Datadog):   tracev1alpha1.Backend_BACKEND_DATADOG,
	string(export.LangSmith): tracev1alpha1.Backend_BACKEND_LANGSMITH,
	string(export.Opik):      tracev1alpha1.Backend_BACKEND_OPIK,
	string(export.Generic):   tracev1alpha1.Backend_BACKEND_GENERIC,
}

var levelToProto = map[string]tracev1alpha1.ContentLevel{
	tracedest.LevelNone:     tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
	tracedest.LevelRedacted: tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED,
	tracedest.LevelFull:     tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL,
}

func backendFromProto(b tracev1alpha1.Backend) string {
	for name, v := range backendToProto {
		if v == b {
			return name
		}
	}
	return ""
}

func levelFromProto(l tracev1alpha1.ContentLevel) string {
	for name, v := range levelToProto {
		if v == l {
			return name
		}
	}
	return ""
}

func testResultProto(t *tracedest.TestResult) *tracev1alpha1.TestResult {
	if t == nil {
		return nil
	}
	return &tracev1alpha1.TestResult{Ok: t.OK, Message: t.Message, TestedAt: timestamppb.New(t.TestedAt)}
}

func traceDestinationProto(d tracedest.Destination) *tracev1alpha1.Destination {
	return &tracev1alpha1.Destination{
		Id: d.ID, Name: d.Name, Backend: backendToProto[d.Backend], EndpointUrl: d.EndpointURL, Params: d.Params,
		SecretFields: d.SecretFields, ResourceAttributes: d.ResourceAttributes, Enabled: d.Enabled,
		LastTest: testResultProto(d.LastTest), CredentialLostAt: optTime(d.CredentialLostAt),
		Health: &tracev1alpha1.Health{
			LastSuccessAt: optTime(d.Health.LastSuccessAt), LastErrorAt: optTime(d.Health.LastErrorAt),
			LastError: d.Health.LastError, SpansDropped: d.Health.SpansDropped,
		},
		CreatedAt: timestamppb.New(d.CreatedAt), UpdatedAt: timestamppb.New(d.UpdatedAt), ConfigVersion: d.ConfigVersion,
	}
}

func traceInput(in *tracev1alpha1.DestinationInput) (tracedest.Input, error) {
	if in == nil {
		return tracedest.Input{}, connect.NewError(connect.CodeInvalidArgument, "destination is required")
	}
	backend := backendFromProto(in.GetBackend())
	if backend == "" {
		return tracedest.Input{}, connect.NewError(connect.CodeInvalidArgument, "backend is required")
	}
	return tracedest.Input{
		Name: in.GetName(), Backend: backend, EndpointURL: in.GetEndpointUrl(), Params: in.GetParams(), Secrets: in.GetSecrets(),
		RemoveSecrets: in.GetRemoveSecrets(), ResourceAttributes: in.GetResourceAttributes(), Enabled: in.GetEnabled(),
	}, nil
}

func traceSettingsProto(v tracedest.Settings) *tracev1alpha1.Settings {
	return &tracev1alpha1.Settings{
		ContentLevel: levelToProto[v.ContentLevel], SampleRate: v.SampleRate, PlatformCopyLevel: levelToProto[v.PlatformCopyLevel],
		Ceiling: levelToProto[v.Ceiling], HasPlatformDestination: v.HasPlatformDestination, UpdatedAt: optTime(v.UpdatedAt), UpdatedBy: v.UpdatedBy,
	}
}

// withReason attaches a machine-readable reason (google.rpc.ErrorInfo) to err, so a client can tell
// apart errors that share a code. The message stays for people and logs.
func withReason(err *connect.Error, reason string, extra ...proto.Message) *connect.Error {
	info := &errdetails.ErrorInfo{Reason: reason, Domain: project.Domain}
	for _, m := range append([]proto.Message{info}, extra...) {
		if d, derr := connectproto.NewErrorDetail(m); derr == nil {
			err = err.WithDetail(d)
		}
	}
	return err
}

// traceError maps the service's errors; the messages are safe to show.
func (a traceAPI) traceError(err error) error {
	var inv *tracedest.InvalidError
	var failed *tracedest.TestFailedError
	switch {
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.As(err, &failed):
		// The whole message is for API users; the outcome alone, as a TestResult, is for a UI.
		return withReason(connect.NewError(connect.CodeFailedPrecondition, failed.Error()), "TEST_FAILED",
			&tracev1alpha1.TestResult{Ok: false, Message: failed.Outcome.Message})
	case errors.Is(err, tracedest.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "not found")
	case errors.Is(err, tracedest.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, "a destination with this name already exists")
	case errors.Is(err, tracedest.ErrChanged):
		return withReason(connect.NewError(connect.CodeAborted, err.Error()), "CHANGED")
	case errors.Is(err, tracedest.ErrOwnerOnly):
		return withReason(connect.NewError(connect.CodePermissionDenied, err.Error()), "OWNER_ONLY")
	case errors.Is(err, tracedest.ErrOwnerAtFull):
		return withReason(connect.NewError(connect.CodePermissionDenied, err.Error()), "OWNER_AT_FULL")
	case errors.Is(err, tracedest.ErrNotMember):
		return withReason(connect.NewError(connect.CodePermissionDenied, err.Error()), "NOT_MEMBER")
	case errors.Is(err, tracedest.ErrAboveCeiling):
		return withReason(connect.NewError(connect.CodeFailedPrecondition, err.Error()), "ABOVE_CEILING")
	case errors.Is(err, tracedest.ErrNoKeys):
		return connect.NewError(connect.CodeUnavailable, err.Error())
	}
	return a.s.dbError(err, "trace destination")
}

// authority is what the caller really is in the org. A platform admin counts as an owner for most
// checks (caller.role), but not here: they may not choose what an org sends off to a backend.
func (c caller) authority() tracedest.Authority {
	role, member := c.p.RoleIn(c.org)
	return tracedest.Authority{Member: member, Owner: member && role == store.OrgRoleOwner}
}

// orgCaller authorizes an admin call and checks the service is wired.
func (a traceAPI) orgCaller(ctx context.Context, orgID string) (caller, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleAdmin)
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() == connect.CodePermissionDenied {
			return caller{}, withReason(ce, "ADMIN_REQUIRED")
		}
		return caller{}, err
	}
	if a.s.traces == nil {
		return caller{}, connect.NewError(connect.CodeUnimplemented, "run traces are not configured")
	}
	return c, nil
}

// platformCaller authorizes a platform admin. Platform changes are audited in the install audit chain (ADR 0100).
func (a traceAPI) platformCaller(ctx context.Context) (*auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if !p.PlatformAdmin {
		return nil, withReason(connect.NewError(connect.CodePermissionDenied, "only a platform admin can do this"), "PLATFORM_ADMIN_REQUIRED")
	}
	if a.s.traces == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "run traces are not configured")
	}
	return p, nil
}

func (a traceAPI) ListDestinations(ctx context.Context, req *tracev1alpha1.ListDestinationsRequest) (*tracev1alpha1.ListDestinationsResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := a.s.traces.List(ctx, c.org)
	if err != nil {
		return nil, a.traceError(err)
	}
	res := &tracev1alpha1.ListDestinationsResponse{}
	for _, d := range list {
		res.Destinations = append(res.Destinations, traceDestinationProto(d))
	}
	return res, nil
}

func (a traceAPI) CreateDestination(ctx context.Context, req *tracev1alpha1.CreateDestinationRequest) (*tracev1alpha1.CreateDestinationResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	in, err := traceInput(req.GetDestination())
	if err != nil {
		return nil, err
	}
	d, err := a.s.traces.Create(requestCtx(ctx), c.org, c.auditActor(), c.actor(), c.authority(), in)
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.CreateDestinationResponse{Destination: traceDestinationProto(d)}, nil
}

func (a traceAPI) UpdateDestination(ctx context.Context, req *tracev1alpha1.UpdateDestinationRequest) (*tracev1alpha1.UpdateDestinationResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	in, err := traceInput(req.GetDestination())
	if err != nil {
		return nil, err
	}
	d, err := a.s.traces.Update(requestCtx(ctx), c.org, c.auditActor(), c.authority(), req.GetDestinationId(), req.ExpectedVersion, in)
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.UpdateDestinationResponse{Destination: traceDestinationProto(d)}, nil
}

func (a traceAPI) DeleteDestination(ctx context.Context, req *tracev1alpha1.DeleteDestinationRequest) (*tracev1alpha1.DeleteDestinationResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := a.s.traces.Delete(requestCtx(ctx), c.org, c.auditActor(), req.GetDestinationId()); err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.DeleteDestinationResponse{}, nil
}

func (a traceAPI) TestDestination(ctx context.Context, req *tracev1alpha1.TestDestinationRequest) (*tracev1alpha1.TestDestinationResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	d, outcome, err := a.s.traces.Test(requestCtx(ctx), c.org, c.auditActor(), req.GetDestinationId())
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.TestDestinationResponse{Destination: traceDestinationProto(d), Result: outcomeProto(outcome)}, nil
}

func outcomeProto(o export.Outcome) *tracev1alpha1.TestResult {
	return &tracev1alpha1.TestResult{Ok: o.OK, Message: o.Message}
}

func (a traceAPI) GetSettings(ctx context.Context, req *tracev1alpha1.GetSettingsRequest) (*tracev1alpha1.GetSettingsResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	v, err := a.s.traces.GetSettings(ctx, c.org)
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.GetSettingsResponse{Settings: traceSettingsProto(v)}, nil
}

func (a traceAPI) UpdateSettings(ctx context.Context, req *tracev1alpha1.UpdateSettingsRequest) (*tracev1alpha1.UpdateSettingsResponse, error) {
	c, err := a.orgCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	level, copyLevel := levelFromProto(req.GetContentLevel()), levelFromProto(req.GetPlatformCopyLevel())
	if level == "" || copyLevel == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "content_level and platform_copy_level are required")
	}
	v, err := a.s.traces.UpdateSettings(requestCtx(ctx), c.org, c.auditActor(), c.authority(), tracedest.SettingsInput{
		ContentLevel: level, SampleRate: req.SampleRate, PlatformCopyLevel: copyLevel,
	})
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.UpdateSettingsResponse{Settings: traceSettingsProto(v)}, nil
}

func (a traceAPI) GetPlatform(ctx context.Context, _ *tracev1alpha1.GetPlatformRequest) (*tracev1alpha1.GetPlatformResponse, error) {
	if _, err := a.platformCaller(ctx); err != nil {
		return nil, err
	}
	v, err := a.s.traces.GetPlatform(ctx)
	if err != nil {
		return nil, a.traceError(err)
	}
	res := &tracev1alpha1.GetPlatformResponse{Ceiling: levelToProto[v.Ceiling]}
	if v.Destination != nil {
		res.Destination = traceDestinationProto(*v.Destination)
	}
	return res, nil
}

func (a traceAPI) SetPlatformDestination(ctx context.Context, req *tracev1alpha1.SetPlatformDestinationRequest) (*tracev1alpha1.SetPlatformDestinationResponse, error) {
	p, err := a.platformCaller(ctx)
	if err != nil {
		return nil, err
	}
	in, err := traceInput(req.GetDestination())
	if err != nil {
		return nil, err
	}
	in.Name = "platform" // the platform destination has one row and no name
	d, err := a.s.traces.SetPlatformDestination(requestCtx(ctx), audit.User(p.UserID), req.ExpectedVersion, in)
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.SetPlatformDestinationResponse{Destination: traceDestinationProto(d)}, nil
}

func (a traceAPI) DeletePlatformDestination(ctx context.Context, _ *tracev1alpha1.DeletePlatformDestinationRequest) (*tracev1alpha1.DeletePlatformDestinationResponse, error) {
	p, err := a.platformCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.s.traces.DeletePlatformDestination(requestCtx(ctx), audit.User(p.UserID)); err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.DeletePlatformDestinationResponse{}, nil
}

func (a traceAPI) TestPlatformDestination(ctx context.Context, _ *tracev1alpha1.TestPlatformDestinationRequest) (*tracev1alpha1.TestPlatformDestinationResponse, error) {
	p, err := a.platformCaller(ctx)
	if err != nil {
		return nil, err
	}
	d, outcome, err := a.s.traces.TestPlatform(requestCtx(ctx), audit.User(p.UserID))
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.TestPlatformDestinationResponse{Destination: traceDestinationProto(d), Result: outcomeProto(outcome)}, nil
}

func (a traceAPI) SetPlatformCeiling(ctx context.Context, req *tracev1alpha1.SetPlatformCeilingRequest) (*tracev1alpha1.SetPlatformCeilingResponse, error) {
	p, err := a.platformCaller(ctx)
	if err != nil {
		return nil, err
	}
	level := levelFromProto(req.GetCeiling())
	if level == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "ceiling is required")
	}
	n, err := a.s.traces.SetPlatformCeiling(requestCtx(ctx), audit.User(p.UserID), level)
	if err != nil {
		return nil, a.traceError(err)
	}
	return &tracev1alpha1.SetPlatformCeilingResponse{Ceiling: levelToProto[level], OrgsLowered: int32(n)}, nil //nolint:gosec // an org count
}
