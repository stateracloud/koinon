// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

// BasePath is the root of the edge. The routes are:
//
//	GET  {BasePath}/agents/{agent}/.well-known/agent-card.json  the agent card (v1.0 and v0.3 union)
//	POST {BasePath}/agents/{agent}/jsonrpc                      A2A v1.0, JSON-RPC binding
//	POST {BasePath}/agents/{agent}/v0.3/jsonrpc                 A2A v0.3, JSON-RPC binding
const BasePath = "/a2a/v1"

// Server serves the edge.
type Server struct {
	be Backend
	h  *Handler
	// publicURL is the origin the card names in its interfaces.
	publicURL string
	// gate authenticates the bearer token and rate limits failures; principal reads the result.
	gate      func(http.Handler) http.Handler
	principal func(context.Context) tokens.Principal
}

// Options configures [New].
type Options struct {
	Backend Backend
	// PublicURL is the external origin of the API, for the URLs in the card.
	PublicURL string
	// Gate wraps a handler with token authentication (adminapi.Server.Gate).
	Gate func(http.Handler) http.Handler
	// Principal reads the authenticated token from the request context (adminapi.PrincipalFrom).
	Principal func(context.Context) tokens.Principal
	// Wait and Poll bound a blocking SendMessage; zero takes [DefaultWait] and [DefaultPoll].
	Wait, Poll time.Duration
}

// New returns a server.
func New(o Options) *Server {
	return &Server{
		be: o.Backend, h: NewHandler(o.Backend, o.Wait, o.Poll), publicURL: strings.TrimSuffix(o.PublicURL, "/"),
		gate: o.Gate, principal: o.Principal,
	}
}

// Mount registers the routes on mux.
func (s *Server) Mount(mux *http.ServeMux) {
	v1 := a2asrv.NewJSONRPCHandler(s.h)
	v0 := a2av0.NewJSONRPCHandler(s.h)
	mux.Handle("GET "+BasePath+"/agents/{agent}/.well-known/agent-card.json", s.gate(s.scoped(http.HandlerFunc(s.card))))
	mux.Handle("POST "+BasePath+"/agents/{agent}/jsonrpc", s.gate(s.scoped(v1)))
	mux.Handle("POST "+BasePath+"/agents/{agent}/v0.3/jsonrpc", s.gate(s.scoped(v0)))
}

// scoped checks the token's role and the agent in the path, and puts the scope in the context.
func (s *Server) scoped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.principal(r.Context())
		if !auth.RoleAtLeast(p.Role, store.OrgRoleMember) {
			http.Error(w, "the token needs the member role or higher", http.StatusForbidden)
			return
		}
		agent, ok := parseUUID(r.PathValue("agent"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		sc := Scope{Org: p.Org, Agent: agent, User: p.CreatedBy}
		if _, err := s.be.Agent(r.Context(), sc); err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRefused) {
				// One answer for both: an agent without the trigger is not an edge agent.
				http.NotFound(w, r)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithScope(r.Context(), sc)))
	})
}

// Card is the agent card of one agent. The interfaces name the two bindings; the security scheme is
// the org API token as a bearer token.
func (s *Server) Card(name, agent string) *a2a.AgentCard {
	base := s.publicURL + BasePath + "/agents/" + agent
	v1 := a2a.NewAgentInterface(base+"/jsonrpc", a2a.TransportProtocolJSONRPC)
	v03 := a2a.NewAgentInterface(base+"/v0.3/jsonrpc", a2a.TransportProtocolJSONRPC)
	v03.ProtocolVersion = a2av0.Version
	return &a2a.AgentCard{
		Name:                name,
		Description:         "A Koinon agent, reached through the Koinon A2A edge.",
		Version:             "1",
		SupportedInterfaces: []*a2a.AgentInterface{v1, v03},
		Capabilities:        a2a.AgentCapabilities{},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID: "chat", Name: "Chat", Description: "Send the agent a task as text. It may ask a question back (input-required).",
			Tags: []string{"chat"},
		}},
		SecuritySchemes:      a2a.NamedSecuritySchemes{"koinon-token": a2a.HTTPAuthSecurityScheme{Scheme: "bearer", Description: "A Koinon org API token with the member role."}},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{"koinon-token": {}}},
	}
}

func (s *Server) card(w http.ResponseWriter, r *http.Request) {
	sc, _ := scopeOf(r.Context())
	a, err := s.be.Agent(r.Context(), sc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	card := s.Card(a.Name, uuidString(sc.Agent))
	body, err := a2av0.NewStaticAgentCardProducer(card).(a2asrv.AgentCardJSONProducer).CardJSON(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// The card is for the caller's token; a shared cache must not keep it.
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(body)
}
