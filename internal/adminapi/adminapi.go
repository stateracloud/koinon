// SPDX-License-Identifier: Apache-2.0

// Package adminapi serves Koinon's admin API: REST under /admin/v1, described by
// api/admin/v1/openapi.yaml (ADR 0056, plan item PV1).
//
// The types and the strict server interface in api.gen.go are generated from that
// document by oapi-codegen (pinned as a tool in go.mod; make generate), so a handler cannot
// drift from the spec without failing to compile, and CI fails when the generated file is
// stale. oapi-codegen does not validate request bodies against the schema's constraints; the
// shared service layer (packages catalog and tokens) does, with the same rules, and the
// tests exercise both.
//
// Authentication is a bearer token only: an org-scoped token with the role admin or owner.
// Cookie sessions are not accepted here, so the API has no CSRF surface. Every call,
// including refused ones, is audited with the token's ID; changes are audited by the service
// layer in the same transaction as the change.
package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	adminv1 "github.com/stateracloud/koinon/api/admin/v1"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/pricing"
)

// BasePath is where the API is mounted.
const BasePath = "/admin/v1"

const maxBody = 64 << 10

// Server implements the admin API.
type Server struct {
	pool    *pgxpool.Pool
	catalog *catalog.Service
	mcp     *mcpservers.Service
	tokens  *tokens.Service
	prices  *prices.Service
	budgets *budget.Service
	// agentModels sets and describes agents' models; triggers fires a schedule trigger now;
	// modelCheck runs the provider echo check.
	agentModels *agentmodel.Service
	// leakGuard changes the org's leak-guard threshold and fetch-trusted host list (old issue 350).
	leakGuard  *orgsettings.Service
	triggers   TriggerRunner
	modelCheck *modelcheck.Service
	// outward is the one switch for outward targets (ADR 0110 decision 5).
	outward *outward.Service
	log     *slog.Logger
	limiter *limiter
	// exportAudit writes an audit export (audit.Export when nil) and exportDone, when set, runs as
	// the export goroutine ends; tests set both through export_test.go.
	exportAudit func(ctx context.Context, w io.Writer, sc audit.ExportScope, fromSeq int64) error
	exportDone  func()
}

var _ StrictServerInterface = (*Server)(nil)

// New returns a Server.
func New(pool *pgxpool.Pool, c *catalog.Service, t *tokens.Service, log *slog.Logger, src pricing.ListSource) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{pool: pool, catalog: c, tokens: t, prices: prices.New(pool, log, src), budgets: budget.New(pool), agentModels: agentmodel.New(pool, prices.NewResolver(src)), leakGuard: orgsettings.NewService(pool), outward: outward.New(pool), log: log, limiter: newLimiter()}
}

type principalKey struct{}

func principalFrom(ctx context.Context) tokens.Principal {
	p, _ := ctx.Value(principalKey{}).(tokens.Principal)
	return p
}

// Mount registers the API on mux under BasePath.
func (s *Server) Mount(mux *http.ServeMux) {
	api := HandlerFromMuxWithBaseURL(NewStrictHandlerWithOptions(s, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadRequest, InvalidRequest, "invalid request: "+err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// A stream that failed after its first byte cannot turn into an error response: the
			// status and part of the body are gone. Appending an error object would end the body
			// cleanly and hand the client a truncated file that looks complete, so the connection
			// is aborted instead (the client sees an unexpected EOF). A streamed body's own failure
			// was logged where it happened (errStreamFailed); anything else is logged here.
			if errors.Is(err, errStreamFailed) {
				panic(http.ErrAbortHandler)
			}
			if rec, ok := w.(*statusRecorder); ok && rec.wrote {
				s.log.Error("admin API response failed after its first byte", "err", err)
				panic(http.ErrAbortHandler)
			}
			s.log.Error("admin API response failed", "err", err)
			writeError(w, http.StatusInternalServerError, Internal, "internal error")
		},
	}), http.NewServeMux(), BasePath)
	mux.HandleFunc("GET "+BasePath+"/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(adminv1.OpenAPI)
	})
	mux.Handle(BasePath+"/", s.authenticate(api))
}

// unavailableRetryAfter is the Retry-After value for HTTP 503 responses. It matches runNowWait
// in internal/services/runnow_durable.go: a busy org queue asks the client to wait that long.
const unavailableRetryAfter = 30

// statusRecorder remembers the status a handler wrote, and whether the response has started.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable && r.Header().Get("Retry-After") == "" {
		r.Header().Set("Retry-After", strconv.Itoa(unavailableRetryAfter))
	}
	r.status, r.wrote = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// errStreamFailed marks a streamed body whose source failed after the response started; the
// failure was logged at its source, and the response error handler aborts the connection.
var errStreamFailed = errors.New("streamed response failed")

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req-" + hex.EncodeToString(b)
}

// authenticate checks the bearer token, stamps a request ID, runs next and audits the call.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newRequestID()
		w.Header().Set("X-Request-Id", reqID)
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="koinon-admin"`)
			s.limiter.fail(limiterKey(clientIP(r), ""), time.Now())
			s.log.Warn("admin API request without a bearer token", "client_ip", clientIP(r), "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, Unauthenticated, "a bearer token is required")
			return
		}
		ip := clientIP(r)
		// A client that keeps sending bad tokens is slowed down, per IP and token prefix (bad tokens
		// whose prefix names no token share one slot per IP). A request with a valid token is
		// never refused for it, so a noisy neighbour behind the same ingress cannot lock admins out.
		rawPrefix := ""
		if rest, ok := strings.CutPrefix(raw, tokens.Prefix); ok && len(rest) >= 8 {
			rawPrefix = rest[:8]
		}
		now := time.Now()
		wait := max(s.limiter.blocked(limiterKey(ip, rawPrefix), now), s.limiter.blocked(limiterKey(ip, ""), now))
		p, err := s.tokens.Authenticate(r.Context(), raw)
		if err != nil {
			var ae *tokens.AuthError
			if !errors.As(err, &ae) {
				// Log a class, never the error: the raw token and header are inputs of this call, and an
				// error value from a lower layer is not ours to vouch for (CodeQL go/clear-text-logging, #152).
				s.log.Error("admin API token check failed", "error_class", errorClass(err))
				writeError(w, http.StatusInternalServerError, Internal, "internal error")
				return
			}
			// Security log: never an org's audit trail, except for a real token (recorded by
			// tokens.Authenticate). The secret part is never logged, only the public prefix.
			key := limiterKey(ip, "")
			if ae.TokenID.Valid {
				key = limiterKey(ip, ae.Prefix)
			}
			s.limiter.fail(key, time.Now())
			if wait > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, http.StatusTooManyRequests, Unauthenticated, "too many failed attempts; retry later")
				return
			}
			s.log.Warn("admin API authentication failed", "client_ip", ip, "token_prefix", ae.Prefix, "reason", ae.Reason, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="koinon-admin", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, Unauthenticated, "the token is invalid, expired or revoked")
			return
		}
		s.limiter.succeed(limiterKey(ip, rawPrefix))
		ctx := audit.WithRequestID(context.WithValue(r.Context(), principalKey{}, p), reqID)
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// The call is audited even when the handler aborts the connection (a stream that failed
		// mid-body panics with http.ErrAbortHandler); the panic then carries on to net/http.
		defer func() {
			v := recover()
			s.auditCall(ctx, p, r, rec.status, v != nil)
			if v != nil {
				panic(v)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// auditCall records one admin API call. It is audited even when the client has gone: detach from
// its context.
func (s *Server) auditCall(ctx context.Context, p tokens.Principal, r *http.Request, status int, aborted bool) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	after := map[string]any{"method": r.Method, "path": r.URL.Path, "status": status}
	if aborted {
		after["aborted"] = true
	}
	if err := audit.RecordAfter(actx, s.pool, audit.Event{
		Org: p.Org, Actor: audit.Token(p.TokenID), Action: "admin_api.call", TargetKind: "admin_api", TargetID: r.URL.Path,
		After: after,
	}); err != nil {
		s.log.Error("could not audit an admin API call", "err", err)
	}
}

// errorClass names the kind of an unexpected token-check failure without carrying its text.
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &pgErr):
		return "database_error_" + pgErr.Code
	case errors.As(err, &netErr):
		return "database_unreachable"
	default:
		return "internal"
	}
}

func writeError(w http.ResponseWriter, status int, code ErrorCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := Error{}
	body.Error.Code, body.Error.Message = code, msg
	_ = jsonEncode(w, body)
}

// problem maps a service-layer error.
func (s *Server) problem(err error) (int, Error) {
	var (
		e    Error
		inv  *catalog.InvalidError
		tin  *tokens.InvalidError
		pin  *prices.InvalidError
		bin  *budget.InvalidError
		ain  *agentmodel.InvalidError
		min  *modelcheck.InvalidError
		mun  *modelcheck.UnavailableError
		busy *modelcheck.BusyError
		oin  *outward.InvalidError
	)
	set := func(status int, code ErrorCode, msg string) (int, Error) {
		e.Error.Code, e.Error.Message = code, msg
		return status, e
	}
	switch {
	case errors.As(err, &inv):
		return set(http.StatusBadRequest, InvalidRequest, inv.Msg)
	case errors.As(err, &tin):
		return set(http.StatusBadRequest, InvalidRequest, tin.Msg)
	case errors.As(err, &pin):
		return set(http.StatusBadRequest, InvalidRequest, pin.Msg)
	case errors.As(err, &bin):
		return set(http.StatusBadRequest, InvalidRequest, bin.Msg)
	case errors.Is(err, audit.ErrBadFilter):
		return set(http.StatusBadRequest, InvalidRequest, err.Error())
	case errors.As(err, &ain):
		return set(http.StatusBadRequest, InvalidRequest, ain.Msg)
	case errors.As(err, &min):
		return set(http.StatusBadRequest, InvalidRequest, min.Msg)
	case errors.As(err, &busy):
		return set(http.StatusConflict, Conflict, busy.Error())
	case errors.As(err, &mun):
		return set(http.StatusServiceUnavailable, Internal, mun.Msg)
	case errors.Is(err, agentmodel.ErrNotFound), errors.Is(err, modelcheck.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return set(http.StatusNotFound, NotFound, "not found")
	case errors.Is(err, triggers.ErrTriggerNotFound):
		return set(http.StatusNotFound, NotFound, err.Error())
	case errors.As(err, &oin):
		return set(http.StatusBadRequest, InvalidRequest, oin.Msg)
	case errors.Is(err, outward.ErrNotFound):
		return set(http.StatusNotFound, NotFound, "not found")
	case errors.Is(err, outward.ErrDeclared), errors.Is(err, outward.ErrDisabled):
		return set(http.StatusConflict, Conflict, err.Error())
	case errors.Is(err, restorehold.ErrHeld):
		return set(http.StatusConflict, Conflict, err.Error())
	case errors.Is(err, triggers.ErrCannotRun):
		return set(http.StatusConflict, Conflict, err.Error())
	case errors.Is(err, budget.ErrForbidden):
		return set(http.StatusForbidden, PermissionDenied, err.Error())
	case errors.Is(err, prices.ErrNotFound), errors.Is(err, budget.ErrNotFound):
		return set(http.StatusNotFound, NotFound, "not found")
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, tokens.ErrNotFound):
		return set(http.StatusNotFound, NotFound, "not found")
	case errors.Is(err, mcpservers.ErrToolChanged):
		return set(http.StatusConflict, Conflict, err.Error())
	case errors.Is(err, catalog.ErrReadOnly):
		return set(http.StatusConflict, Conflict, err.Error())
	case errors.Is(err, catalog.ErrConflict):
		return set(http.StatusConflict, Conflict, "an entry with this name already exists")
	case errors.Is(err, catalog.ErrNoKeys):
		return set(http.StatusServiceUnavailable, Internal, err.Error())
	case connect.CodeOf(err) == connect.CodeUnavailable:
		msg := err.Error()
		var ce *connect.Error
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		s.log.Warn("admin API unavailable", "err", err)
		return set(http.StatusServiceUnavailable, Unavailable, msg)
	}
	s.log.Error("admin API error", "err", err)
	return set(http.StatusInternalServerError, Internal, "internal error")
}

func uid(id openapi_types.UUID) string { return id.String() }

func pgUUID(id string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(id)
	return u
}

func toProvider(p catalog.Provider) Provider {
	out := Provider{
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, HasKey: p.HasKey, Name: p.Name, Type: p.Type, Url: p.URL,
		Id: openapi_types.UUID(pgUUID(p.ID).Bytes), Source: Source(p.Source),
	}
	if p.ManagedBySecret != "" {
		out.ManagedBySecret = &p.ManagedBySecret
	}
	if p.DisabledReason != "" {
		out.DisabledReason = &p.DisabledReason
	}
	if p.AnthropicWorkspaceID != "" {
		out.AnthropicWorkspaceId = &p.AnthropicWorkspaceID
	}
	out.CredentialLostAt = p.CredentialLostAt
	out.AdminDisabled = adminDisabledJSON(p.AdminDisabled)
	out.HasCostKey = p.HasCostKey
	if p.CostScope != "" {
		out.CostScope = &p.CostScope
	}
	out.CostKeyLostAt = p.CostKeyLostAt
	out.Formats = append([]ModelFormat{}, p.Formats...)
	return out
}

func toModel(m catalog.Model) Model {
	out := Model{
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Guard: m.Guard, Name: m.Name, Source: Source(m.Source),
		Format: m.Format, FormatOverridden: m.FormatOverridden,
		Id: openapi_types.UUID(pgUUID(m.ID).Bytes), ProviderId: openapi_types.UUID(pgUUID(m.ProviderID).Bytes),
	}
	if m.ManagedBySecret != "" {
		out.ManagedBySecret = &m.ManagedBySecret
	}
	if m.DisabledReason != "" {
		out.DisabledReason = &m.DisabledReason
	}
	return out
}

// fromRates carries the deprecated rates field to the catalog, which refuses it.
func fromRates(r *Rates) *catalog.Rates {
	if r == nil {
		return nil
	}
	out := &catalog.Rates{Input: r.Input, Output: r.Output}
	if r.CacheRead != nil {
		out.CacheRead = *r.CacheRead
	}
	return out
}

func (s *Server) actor(ctx context.Context) (tokens.Principal, audit.Actor) {
	p := principalFrom(ctx)
	return p, audit.Token(p.TokenID)
}

// ListProviders implements StrictServerInterface.
func (s *Server) ListProviders(ctx context.Context, _ ListProvidersRequestObject) (ListProvidersResponseObject, error) {
	p, _ := s.actor(ctx)
	list, err := s.catalog.ListProviders(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListProvidersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	checks, err := s.echoChecks(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListProvidersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListProviders200JSONResponse{Providers: []Provider{}}
	for _, x := range list {
		pv := toProvider(x)
		if r, ok := checks[pgUUID(x.ID)]; ok {
			e := toEchoCheck(r)
			pv.EchoCheck = &e
		}
		out.Providers = append(out.Providers, pv)
	}
	return out, nil
}

// CreateProvider implements StrictServerInterface.
func (s *Server) CreateProvider(ctx context.Context, req CreateProviderRequestObject) (CreateProviderResponseObject, error) {
	p, actor := s.actor(ctx)
	in := catalog.ProviderInput{Name: req.Body.Name, Type: req.Body.Type, URL: req.Body.Url}
	if req.Body.ApiKey != nil {
		in.APIKey = *req.Body.ApiKey
	}
	if req.Body.AnthropicWorkspaceId != nil {
		in.AnthropicWorkspaceID = *req.Body.AnthropicWorkspaceId
	}
	if req.Body.CostApiKey != nil {
		in.CostAPIKey = *req.Body.CostApiKey
	}
	if req.Body.CostScope != nil {
		in.CostScope = *req.Body.CostScope
	}
	got, err := s.catalog.CreateProvider(ctx, p.Org, actor, pgtype.UUID{}, in)
	if err != nil {
		st, e := s.problem(err)
		return CreateProviderdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return CreateProvider201JSONResponse(toProvider(got)), nil
}

// GetProvider implements StrictServerInterface.
func (s *Server) GetProvider(ctx context.Context, req GetProviderRequestObject) (GetProviderResponseObject, error) {
	p, _ := s.actor(ctx)
	got, err := s.catalog.GetProvider(ctx, p.Org, uid(req.ProviderId))
	if err != nil {
		st, e := s.problem(err)
		return GetProviderdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	pv := toProvider(got)
	if checks, err := s.echoChecks(ctx, p.Org); err != nil {
		st, e := s.problem(err)
		return GetProviderdefaultJSONResponse{Body: e, StatusCode: st}, nil
	} else if r, ok := checks[pgUUID(got.ID)]; ok {
		e := toEchoCheck(r)
		pv.EchoCheck = &e
	}
	return GetProvider200JSONResponse(pv), nil
}

// UpdateProvider implements StrictServerInterface.
func (s *Server) UpdateProvider(ctx context.Context, req UpdateProviderRequestObject) (UpdateProviderResponseObject, error) {
	p, actor := s.actor(ctx)
	got, err := s.catalog.UpdateProvider(ctx, p.Org, actor, uid(req.ProviderId),
		catalog.ProviderPatch{
			Type: req.Body.Type, URL: req.Body.Url, APIKey: req.Body.ApiKey, AnthropicWorkspaceID: req.Body.AnthropicWorkspaceId,
			CostAPIKey: req.Body.CostApiKey, CostScope: req.Body.CostScope,
		})
	if err != nil {
		st, e := s.problem(err)
		return UpdateProviderdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return UpdateProvider200JSONResponse(toProvider(got)), nil
}

// DeleteProvider implements StrictServerInterface.
func (s *Server) DeleteProvider(ctx context.Context, req DeleteProviderRequestObject) (DeleteProviderResponseObject, error) {
	p, actor := s.actor(ctx)
	if err := s.catalog.DeleteProvider(ctx, p.Org, actor, uid(req.ProviderId)); err != nil {
		st, e := s.problem(err)
		return DeleteProviderdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return DeleteProvider204Response{}, nil
}

// ListModels implements StrictServerInterface.
func (s *Server) ListModels(ctx context.Context, _ ListModelsRequestObject) (ListModelsResponseObject, error) {
	p, _ := s.actor(ctx)
	list, err := s.catalog.ListModels(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListModelsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListModels200JSONResponse{Models: []Model{}}
	for _, x := range list {
		out.Models = append(out.Models, toModel(x))
	}
	return out, nil
}

// GetDefaultModel implements StrictServerInterface.
func (s *Server) GetDefaultModel(ctx context.Context, _ GetDefaultModelRequestObject) (GetDefaultModelResponseObject, error) {
	p, _ := s.actor(ctx)
	m, err := s.agentModels.GetDefault(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return GetDefaultModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetDefaultModel200JSONResponse(defaultModel(m)), nil
}

// SetDefaultModel implements StrictServerInterface.
func (s *Server) SetDefaultModel(ctx context.Context, req SetDefaultModelRequestObject) (SetDefaultModelResponseObject, error) {
	p, actor := s.actor(ctx)
	want := ""
	if req.Body.Model != nil {
		want = *req.Body.Model
	}
	if err := s.agentModels.SetDefault(ctx, p.Org, actor, want, time.Now()); err != nil {
		st, e := s.problem(err)
		return SetDefaultModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetDefaultModel200JSONResponse(defaultModel(want)), nil
}

func defaultModel(m string) DefaultModel {
	if m == "" {
		return DefaultModel{}
	}
	return DefaultModel{Model: &m}
}

// CreateModel implements StrictServerInterface.
func (s *Server) CreateModel(ctx context.Context, req CreateModelRequestObject) (CreateModelResponseObject, error) {
	p, actor := s.actor(ctx)
	in := catalog.ModelInput{ProviderID: uid(req.Body.ProviderId), Name: req.Body.Name, Rates: fromRates(req.Body.Rates)}
	if req.Body.Guard != nil {
		in.Guard = *req.Body.Guard
	}
	if req.Body.Format != nil {
		in.Format = *req.Body.Format
	}
	got, err := s.catalog.CreateModel(ctx, p.Org, actor, pgtype.UUID{}, in)
	if err != nil {
		st, e := s.problem(err)
		return CreateModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	var out ModelCreated
	if err := via(toModel(got), &out); err != nil {
		st, e := s.problem(err)
		return CreateModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	if w := s.prices.ModelWarnings(ctx, p.Org, pgUUID(got.ProviderID), got.Name, modelformat.Format(got.Format)); len(w) > 0 {
		out.Warnings = &w
	}
	return CreateModel201JSONResponse(out), nil
}

// GetModel implements StrictServerInterface.
func (s *Server) GetModel(ctx context.Context, req GetModelRequestObject) (GetModelResponseObject, error) {
	p, _ := s.actor(ctx)
	got, err := s.catalog.GetModel(ctx, p.Org, uid(req.ModelId))
	if err != nil {
		st, e := s.problem(err)
		return GetModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetModel200JSONResponse(toModel(got)), nil
}

// UpdateModel implements StrictServerInterface.
func (s *Server) UpdateModel(ctx context.Context, req UpdateModelRequestObject) (UpdateModelResponseObject, error) {
	p, actor := s.actor(ctx)
	patch := catalog.ModelPatch{Guard: req.Body.Guard, Format: req.Body.Format, Rates: fromRates(req.Body.Rates)}
	patch.ClearRates = req.Body.ClearRates != nil && *req.Body.ClearRates
	got, err := s.catalog.UpdateModel(ctx, p.Org, actor, uid(req.ModelId), patch)
	if err != nil {
		st, e := s.problem(err)
		return UpdateModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return UpdateModel200JSONResponse(toModel(got)), nil
}

// DeleteModel implements StrictServerInterface.
func (s *Server) DeleteModel(ctx context.Context, req DeleteModelRequestObject) (DeleteModelResponseObject, error) {
	p, actor := s.actor(ctx)
	if err := s.catalog.DeleteModel(ctx, p.Org, actor, uid(req.ModelId)); err != nil {
		st, e := s.problem(err)
		return DeleteModeldefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return DeleteModel204Response{}, nil
}

func toToken(t tokens.Token) Token {
	out := Token{
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Name: t.Name, Prefix: t.Prefix, Role: Role(t.Role),
		Id: openapi_types.UUID(pgUUID(t.ID).Bytes), RevokedAt: t.RevokedAt, LastUsedAt: t.LastUsedAt,
	}
	return out
}

// ListTokens implements StrictServerInterface.
func (s *Server) ListTokens(ctx context.Context, _ ListTokensRequestObject) (ListTokensResponseObject, error) {
	p, _ := s.actor(ctx)
	list, err := s.tokens.List(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListTokensdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListTokens200JSONResponse{Tokens: []Token{}}
	for _, x := range list {
		out.Tokens = append(out.Tokens, toToken(x))
	}
	return out, nil
}

// CreateToken implements StrictServerInterface. The new token carries at most the calling
// token's role.
func (s *Server) CreateToken(ctx context.Context, req CreateTokenRequestObject) (CreateTokenResponseObject, error) {
	p, actor := s.actor(ctx)
	var ttl time.Duration
	if req.Body.ExpiresIn != nil {
		ttl = time.Duration(*req.Body.ExpiresIn) * time.Second
	}
	created, err := s.tokens.Create(ctx, p.Org, actor, tokens.Creator{User: p.CreatedBy, Role: p.Role, Parent: &p}, req.Body.Name, store.OrgRole(req.Body.Role), ttl)
	if err != nil {
		st, e := s.problem(err)
		return CreateTokendefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return CreateToken201JSONResponse{Token: toToken(created.Token), Secret: created.Secret}, nil
}

// RevokeToken implements StrictServerInterface.
func (s *Server) RevokeToken(ctx context.Context, req RevokeTokenRequestObject) (RevokeTokenResponseObject, error) {
	p, actor := s.actor(ctx)
	if _, err := s.tokens.Revoke(ctx, p.Org, actor, p.Role, uid(req.TokenId)); err != nil {
		st, e := s.problem(err)
		return RevokeTokendefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return RevokeToken204Response{}, nil
}

// templateLimit is the spendUSD of an agent version, the default daily limit of an agent that has none of its own.
func (s *Server) templateLimit(ctx context.Context, org, version pgtype.UUID) money.USD {
	v, err := store.New(s.pool).GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: version})
	if err != nil {
		return 0
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return 0
	}
	return money.FromCents(cfg.Limits.SpendUSD.Cents())
}
