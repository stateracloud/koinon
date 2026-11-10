// SPDX-License-Identifier: Apache-2.0

// Package e2eclient is the logged-in API client the end-to-end suites share: test/e2e (the
// quickstart) and test/drill (the restore drill, ADR 0109). It speaks the public ConnectRPC API
// with a session cookie, and /admin/v1 with a token, exactly as a user or a script would.
package e2eclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1/taskv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
)

// sessionTransport keeps the session cookie the way a browser would. The cookie is Secure, and
// net/http's cookie jar never sends Secure cookies over http://, which the port-forward is.
type sessionTransport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	token string
}

func (t *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	token := t.token
	t.mu.Unlock()
	if token != "" {
		req = req.Clone(req.Context())
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			t.mu.Lock()
			t.token = c.Value
			t.mu.Unlock()
		}
	}
	return res, nil
}

// SessionToken returns the session cookie value the client holds ("" before a login).
func (c *Client) SessionToken() string {
	if t, ok := c.HTTP.Transport.(*sessionTransport); ok {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.token
	}
	return ""
}

// SetSession makes the client send token as its session cookie, as a browser that kept it would.
func (c *Client) SetSession(token string) {
	if t, ok := c.HTTP.Transport.(*sessionTransport); ok {
		t.mu.Lock()
		t.token = token
		t.mu.Unlock()
	}
}

// Client is one logged-in user of the API.
type Client struct {
	Base        string
	HTTP        *http.Client
	Auth        authv1alpha1connect.AuthServiceClient
	Connections connectionv1alpha1connect.ConnectionServiceClient
	Templates   templatev1alpha1connect.TemplateServiceClient
	Agents      agentv1alpha1connect.AgentServiceClient
	Tasks       taskv1alpha1connect.TaskServiceClient
	Runs        runv1alpha1connect.RunServiceClient
	Approvals   approvalv1alpha1connect.ApprovalServiceClient
	Tokens      tokenv1alpha1connect.TokenServiceClient
}

// New returns a client for the API at base (for example http://127.0.0.1:8080), not logged in.
func New(base string) *Client {
	hc := &http.Client{Transport: &sessionTransport{base: http.DefaultTransport}}
	c := connect.NewClient(connecthttp.NewTransport(hc, base))
	return &Client{
		Base:        base,
		HTTP:        hc,
		Auth:        authv1alpha1connect.NewAuthServiceClient(c),
		Connections: connectionv1alpha1connect.NewConnectionServiceClient(c),
		Templates:   templatev1alpha1connect.NewTemplateServiceClient(c),
		Agents:      agentv1alpha1connect.NewAgentServiceClient(c),
		Tasks:       taskv1alpha1connect.NewTaskServiceClient(c),
		Runs:        runv1alpha1connect.NewRunServiceClient(c),
		Approvals:   approvalv1alpha1connect.NewApprovalServiceClient(c),
		Tokens:      tokenv1alpha1connect.NewTokenServiceClient(c),
	}
}

// Login posts the login form as the web UI does and returns passwordChangeRequired. A refusal
// (any status but 200) is an error that carries the status and the body.
func (c *Client) Login(ctx context.Context, username, password string) (bool, error) {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/auth/login", strings.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("login: status %d: %s", res.StatusCode, data)
	}
	var out struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false, fmt.Errorf("login: %w", err)
	}
	return out.PasswordChangeRequired, nil
}

// Admin calls /admin/v1 with a bearer token (made with Tokens.CreateToken) and returns the status
// and the body. body is optional and sent as JSON.
func (c *Client) Admin(ctx context.Context, token, method, path string, body ...string) (int, []byte, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = strings.NewReader(body[0])
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/admin/v1"+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, nil
}

// FakeModelRequest is one request the fake model server received (GET /_fake/requests?detail=1).
type FakeModelRequest struct {
	Path    string      `json:"path"`
	Model   string      `json:"model"`
	Query   string      `json:"query"`
	Headers http.Header `json:"headers"`
}

// FakeModelRequests reads the fake model server's request log at base, with every header.
func FakeModelRequests(ctx context.Context, base string) ([]FakeModelRequest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/_fake/requests?detail=1", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fake model requests: status %d", res.StatusCode)
	}
	var out []FakeModelRequest
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
