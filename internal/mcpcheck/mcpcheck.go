// SPDX-License-Identifier: Apache-2.0

// Package mcpcheck is the live check of an agent's MCP tools (old issue 488, ADR 0057 amendment
// 2026-11-01).
//
// Before a run starts its harness, koinon-agentd wants to know which MCP tools the gateway really
// lists for the agent, not what the prober saw up to a minute ago. The only credential that proves
// the agent's own grants is the agent's gateway key, and that key must not reach koinon-agentd, a
// sandbox, an environment, an image or a log. So koinon-agentd asks koinon-api over the saved-work
// listener (as for the home repository password, package checkpoint): koinon-api reads the agent's
// key Secret, runs tools/list through the gateway as the agent, and answers with the tool names
// only. The request is authenticated with an HMAC made with the same key as the saved-work posts,
// over a domain-separated message, good for one agent for a minute.
package mcpcheck

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/execution"
)

const (
	// Path is where the Server listens (on the saved-work listener).
	Path = "/internal/mcp-tools"
	// TokenLifetime is how long a request's credential is good for.
	TokenLifetime = time.Minute
	// KeyDataKey is the data key of the agent's key Secret (the reconciler writes it).
	KeyDataKey = "api_key"

	maxPages    = 20
	listTimeout = 30 * time.Second
)

// Token is the bearer credential for one request: an HMAC over the org, the agent and the expiry,
// with a prefix that keeps it from being any other internal credential.
func Token(key []byte, org, agent string, exp int64) string {
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "mcp-tools\n%s\n%s\n%d", org, agent, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// ValidToken reports whether token matches and exp is neither past nor too far away.
func ValidToken(key []byte, token, org, agent string, exp int64, now time.Time) bool {
	if exp < now.Unix() || exp > now.Add(TokenLifetime).Unix() {
		return false
	}
	return hmac.Equal([]byte(token), []byte(Token(key, org, agent, exp)))
}

// Reply is the answer: the names the gateway listed, with its prefixes (<target>_<tool>). It never
// holds a credential.
type Reply struct {
	Tools []string `json:"tools"`
}

// AgentKeys yields an agent's gateway key.
type AgentKeys interface {
	AgentKey(ctx context.Context, org, agent pgtype.UUID) (string, error)
}

// KubeAgentKeys reads an agent's key from its Secret in one namespace.
type KubeAgentKeys struct {
	Client    kubernetes.Interface
	Namespace string
}

// AgentKey implements AgentKeys.
func (k KubeAgentKeys) AgentKey(ctx context.Context, org, agent pgtype.UUID) (string, error) {
	name := execution.AgentObjectNames(org.Bytes, agent.Bytes).KeySecret
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the agent's key from the Secret %s: %w", name, err)
	}
	if len(s.Data[KeyDataKey]) == 0 {
		return "", fmt.Errorf("the Secret %s holds no key", name)
	}
	return string(s.Data[KeyDataKey]), nil
}

type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(req)
}

// ListTools lists the tools the gateway at endpoint (its MCP URL) offers the holder of key: the
// agent's own view, after the gateway's authorization rules. Errors never carry the key.
func ListTools(ctx context.Context, endpoint, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-mcp-check", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearerTransport{key: key, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the gateway: %w", err)
	}
	defer func() { _ = session.Close() }()
	var out []string
	var cursor string
	for range maxPages {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		for _, t := range res.Tools {
			out = append(out, t.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// Server answers Path in koinon-api, which may read the agents' key Secrets.
type Server struct {
	Keys AgentKeys
	// Gateway is agentgateway's MCP endpoint as koinon-api reaches it.
	Gateway string
	// List defaults to ListTools; tests replace it.
	List func(ctx context.Context, endpoint, key string) ([]string, error)
	// Key is the HMAC key shared with koinon-agentd.
	Key func(context.Context) ([]byte, error)
	Log *slog.Logger
	Now func() time.Time
}

// ServeHTTP implements http.Handler.
func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log, now, list := s.Log, s.Now, s.List
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if list == nil {
		list = ListTools
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var org, agent pgtype.UUID
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if org.Scan(q.Get("org")) != nil || agent.Scan(q.Get("agent")) != nil || !org.Valid || !agent.Valid || err != nil {
		http.Error(w, "bad parameters", http.StatusBadRequest)
		return
	}
	key, err := s.Key(r.Context())
	if err != nil {
		log.Error("mcp check: reading the request key failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ValidToken(key, token, org.String(), agent.String(), exp, now()) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return
	}
	if s.Gateway == "" {
		http.Error(w, "the live check is not configured on this koinon-api", http.StatusServiceUnavailable)
		return
	}
	agentKey, err := s.Keys.AgentKey(r.Context(), org, agent)
	if err != nil {
		log.Warn("mcp check: the agent's key is not available", "org", org.String(), "agent", agent.String(), "err", err)
		http.Error(w, "the agent has no gateway key yet", http.StatusConflict)
		return
	}
	tools, err := list(r.Context(), s.Gateway, agentKey)
	if err != nil {
		log.Warn("mcp check: listing the agent's tools failed", "org", org.String(), "agent", agent.String(), "err", err)
		http.Error(w, "the gateway did not list the agent's tools", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Reply{Tools: tools})
}

// Remote is the agentd side: it asks koinon-api for the agent's tool list and holds no key of the
// agent. It satisfies agentd.MCPTools.
type Remote struct {
	HTTP    *http.Client
	BaseURL string
	Key     func() ([]byte, error)
	Now     func() time.Time
}

// Tools returns the names the gateway lists for the agent right now.
func (c Remote) Tools(ctx context.Context, org, agent pgtype.UUID) ([]string, error) {
	key, err := c.Key()
	if err != nil || len(key) == 0 {
		return nil, errors.New("the key that signs MCP check requests is unreadable")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	exp := now().Add(TokenLifetime / 2).Unix()
	q := url.Values{"org": {org.String()}, "agent": {agent.String()}, "exp": {strconv.FormatInt(exp, 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+Path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+Token(key, org.String(), agent.String(), exp))
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking koinon-api for the agent's MCP tools: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("koinon-api answered %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var out Reply
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("koinon-api answered with an unreadable tool list")
	}
	return out.Tools, nil
}
