// SPDX-License-Identifier: Apache-2.0

// Package approvalroute is the approval route of Koinon's own gateway (ADR 0097 decision 2.3, #1301).
//
// The leak guard runs inside koinon-gateway (mcpguard.GatewayGuard). When it holds a call for a person,
// the gateway does not write the approval itself: its database login has no right on the approval tables
// (migrations.GrantGateway). Client is the guard's mcpguard.ApproveFunc in the gateway: it asks koinon-api
// over Koinon RPC. Server is koinon-api's side: it checks the call against its own rows and records the
// ADR 0042 approval with the same code as the agentgateway path (mcpask.Server.RequestApproval), so both
// drivers give the agent the same answer and the person the same card.
//
// The route shares the internal listener of the State stream and its token. koinon-api does not trust the
// names in a request: the org, the agent, the server, the agent's allowance of the server and the
// published tool are read again, and a call that does not match them is refused. Any failure fails the
// call closed: the guard refuses a call whose approval it could not record.
package approvalroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1/gatewayv1alpha1connect"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpguard"
)

const (
	// Prefix is the URL path prefix of the service's procedures.
	Prefix = "/koinon.gateway.v1alpha1.GatewayApprovalService/"
	// MaxRiskBytes bounds the guard's risk object in one request.
	MaxRiskBytes = 64 << 10
	// maxMessageBytes bounds one request message: the arguments, the risk and the names.
	maxMessageBytes = mcpask.MaxArgsBytes + MaxRiskBytes + 4<<10
	// DefaultTimeout bounds one request of the Client.
	DefaultTimeout = 10 * time.Second
	// maxNameLen bounds the org slug and the tool name.
	maxNameLen = 256
)

// ServerConfig configures a Server.
type ServerConfig struct {
	// DB reads the org, agent, server, allowance and tool rows. Required.
	DB store.DBTX
	// Approve records the approval (mcpask.Server.RequestApproval). Required.
	Approve mcpguard.ApproveFunc
	// Auth checks the gateway's token. Required: the route is never served open.
	Auth func(http.Handler) http.Handler
	Log  *slog.Logger
}

// Server is koinon-api's side of the route.
type Server struct {
	q       *store.Queries
	approve mcpguard.ApproveFunc
	log     *slog.Logger
	handler http.Handler
}

// NewServer returns a Server.
func NewServer(cfg ServerConfig) (*Server, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("approvalroute: a database is required")
	case cfg.Approve == nil:
		return nil, errors.New("approvalroute: an approve function is required")
	case cfg.Auth == nil:
		return nil, errors.New("approvalroute: an authentication wrapper is required: the route is never served open")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{q: store.New(cfg.DB), approve: cfg.Approve, log: log}
	srv := connect.NewServer()
	gatewayv1alpha1connect.RegisterGatewayApprovalServiceHandler(srv, rpcHandler{s})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv, connecthttp.WithReadMaxBytes(maxMessageBytes))
	s.handler = cfg.Auth(mux)
	return s, nil
}

// Mount returns a wrapper for the internal listener: requests under Prefix go to the service, behind
// ServerConfig.Auth; every other request goes to next, untouched.
func (s *Server) Mount() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, Prefix) {
				s.handler.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type rpcHandler struct{ s *Server }

var _ gatewayv1alpha1connect.GatewayApprovalServiceHandler = rpcHandler{}

// errNotMatched is the one answer for a call that does not match koinon-api's rows. It does not say
// which row: the caller learns nothing about other orgs.
var errNotMatched = connect.NewError(connect.CodePermissionDenied, "the call does not match an agent's allowed, published tool")

func (h rpcHandler) RequestApproval(ctx context.Context, req *gwv1.RequestApprovalRequest) (*gwv1.RequestApprovalResponse, error) {
	s := h.s
	slug, tool := req.GetOrg(), req.GetTool()
	if slug == "" || len(slug) > maxNameLen || tool == "" || len(tool) > maxNameLen {
		return nil, connect.NewError(connect.CodeInvalidArgument, "org and tool must be 1 to 256 characters")
	}
	var agentID, serverID pgtype.UUID
	if agentID.Scan(req.GetAgentId()) != nil || serverID.Scan(req.GetServerId()) != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "agent_id and server_id must be UUIDs")
	}
	args := json.RawMessage(req.GetArguments())
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	risk, err := decodeRisk(req.GetRisk())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}

	org, err := s.q.GetOrgBySlug(ctx, slug)
	if err != nil {
		return nil, s.lookupErr(err, "org")
	}
	agent, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: org.ID, ID: agentID})
	if err != nil {
		return nil, s.lookupErr(err, "agent")
	}
	srv, err := s.q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org.ID, ID: serverID})
	if err != nil {
		return nil, s.lookupErr(err, "server")
	}
	allowed, err := s.q.GetAgentMCPServer(ctx, store.GetAgentMCPServerParams{OrgID: org.ID, AgentID: agent.ID, ServerID: srv.ID})
	if err != nil {
		return nil, s.lookupErr(err, "allowance")
	}
	if allowed.Tools != nil && !slices.Contains(allowed.Tools, tool) {
		return nil, errNotMatched
	}
	t, err := s.q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: org.ID, ServerID: srv.ID, Name: tool})
	if err != nil {
		return nil, s.lookupErr(err, "tool")
	}

	text, err := s.approve(ctx, org, agent, srv, t, args, risk)
	if err != nil {
		// The text can name the cause (no running run); the log keeps the detail. The gateway fails the
		// call closed either way.
		s.log.Warn("gateway approval route: recording the approval failed", "org", org.Slug, "agent", agent.ID.String(), "tool", tool, "err", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the approval could not be recorded")
	}
	return &gwv1.RequestApprovalResponse{Text: text}, nil
}

func (s *Server) lookupErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotMatched
	}
	s.log.Error("gateway approval route: reading a row failed", "what", what, "err", err)
	return connect.NewError(connect.CodeInternal, "internal error")
}

// decodeRisk reads the guard's risk object. Numbers keep their text, so the card shows what the guard
// wrote. Empty is no risk object; the card still marks the approval as the leak guard's.
func decodeRisk(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if len(raw) > MaxRiskBytes {
		return nil, fmt.Errorf("risk is larger than %d bytes", MaxRiskBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var risk map[string]any
	if err := dec.Decode(&risk); err != nil || risk == nil {
		return nil, errors.New("risk must be a JSON object")
	}
	if dec.More() {
		return nil, errors.New("risk must be one JSON object")
	}
	return risk, nil
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// URL is the base URL of koinon-api's internal listener (the State stream's).
	URL string
	// Token returns the gateway's bearer token; it is read on every request.
	Token func() (string, error)
	// HTTP is the base client; nil is a plain one.
	HTTP *http.Client
	// Timeout bounds one request (DefaultTimeout when <= 0).
	Timeout time.Duration
}

// Client is the gateway's side of the route.
type Client struct {
	api     gatewayv1alpha1connect.GatewayApprovalServiceClient
	timeout time.Duration
}

// NewClient returns a Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("approvalroute: the URL of koinon-api is required")
	}
	if cfg.Token == nil {
		return nil, errors.New("approvalroute: a token source is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	hc := &http.Client{}
	if cfg.HTTP != nil {
		c := *cfg.HTTP
		hc = &c
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = bearer{base: base, token: cfg.Token}
	return &Client{
		api:     gatewayv1alpha1connect.NewGatewayApprovalServiceClient(connect.NewClient(connecthttp.NewTransport(hc, cfg.URL, connecthttp.WithSendMaxBytes(maxMessageBytes)))),
		timeout: cfg.Timeout,
	}, nil
}

var _ mcpguard.ApproveFunc = (*Client)(nil).Approve

// Approve implements mcpguard.ApproveFunc: it asks koinon-api to record the approval of exactly this call
// and returns the text the agent reads. An error fails the call closed.
func (c *Client) Approve(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool store.McpTool, args json.RawMessage, risk map[string]any) (string, error) {
	var riskJSON []byte
	if risk != nil {
		b, err := json.Marshal(risk)
		if err != nil {
			return "", fmt.Errorf("approvalroute: encode the risk: %w", err)
		}
		riskJSON = b
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	res, err := c.api.RequestApproval(ctx, &gwv1.RequestApprovalRequest{
		Org: org.Slug, AgentId: agent.ID.String(), ServerId: srv.ID.String(), Tool: tool.Name,
		Arguments: args, Risk: riskJSON,
	})
	if err != nil {
		return "", fmt.Errorf("approvalroute: koinon-api did not record the approval: %w", err)
	}
	if res.GetText() == "" {
		return "", errors.New("approvalroute: koinon-api returned no text")
	}
	return res.GetText(), nil
}

type bearer struct {
	base  http.RoundTripper
	token func() (string, error)
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := b.token()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(r)
}
