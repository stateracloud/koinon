// SPDX-License-Identifier: Apache-2.0

package e2e_test

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
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
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

// client is one logged-in user of the API.
type client struct {
	base        string
	http        *http.Client
	auth        authv1alpha1connect.AuthServiceClient
	connections connectionv1alpha1connect.ConnectionServiceClient
	templates   templatev1alpha1connect.TemplateServiceClient
	agents      agentv1alpha1connect.AgentServiceClient
	tickets     ticketv1alpha1connect.TicketServiceClient
	runs        runv1alpha1connect.RunServiceClient
	approvals   approvalv1alpha1connect.ApprovalServiceClient
}

func newClient(base string) *client {
	hc := &http.Client{Transport: &sessionTransport{base: http.DefaultTransport}}
	c := connect.NewClient(connecthttp.NewTransport(hc, base))
	return &client{
		base:        base,
		http:        hc,
		auth:        authv1alpha1connect.NewAuthServiceClient(c),
		connections: connectionv1alpha1connect.NewConnectionServiceClient(c),
		templates:   templatev1alpha1connect.NewTemplateServiceClient(c),
		agents:      agentv1alpha1connect.NewAgentServiceClient(c),
		tickets:     ticketv1alpha1connect.NewTicketServiceClient(c),
		runs:        runv1alpha1connect.NewRunServiceClient(c),
		approvals:   approvalv1alpha1connect.NewApprovalServiceClient(c),
	}
}

// login posts the login form as the web UI does and returns passwordChangeRequired.
func (c *client) login(ctx context.Context, username, password string) bool {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/auth/login", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	Expect(res.StatusCode).To(Equal(http.StatusOK), "login: %s", data)
	var out struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
	}
	Expect(json.Unmarshal(data, &out)).To(Succeed())
	return out.PasswordChangeRequired
}

// fakeGitHub reads the fake GitHub's test-only endpoints (cmd/koinon-fakegithub).
type fakeGitHub struct{ base string }

type fakeApp struct {
	AppID          int64  `json:"appId"`
	InstallationID int64  `json:"installationId"`
	Account        string `json:"account"`
	PrivateKey     string `json:"privateKey"`
}

type fakePull struct {
	Number  int64
	Title   string
	Body    string
	Head    string
	Base    string
	HeadSHA string
	HTMLURL string
}

type fakeRepo struct {
	Refs  map[string]string            `json:"refs"`
	Pulls []fakePull                   `json:"pulls"`
	Calls map[string]int               `json:"calls"`
	Files map[string]map[string]string `json:"files"`
	Mints int                          `json:"mints"`
}

func (f fakeGitHub) get(ctx context.Context, path string, out any) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+path, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	Expect(res.StatusCode).To(Equal(http.StatusOK))
	Expect(json.NewDecoder(res.Body).Decode(out)).To(Succeed())
}

func (f fakeGitHub) app(ctx context.Context) fakeApp {
	var a fakeApp
	f.get(ctx, "/_fake/app", &a)
	return a
}

func (f fakeGitHub) repo(ctx context.Context, name string) fakeRepo {
	var r fakeRepo
	f.get(ctx, "/_fake/repos/"+name, &r)
	return r
}
