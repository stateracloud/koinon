// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/catalog"
)

// OAuthMessage is what an admin reads when a server needs per-user OAuth, which v0.1 does not do.
const OAuthMessage = "this server asks each user to sign in with OAuth, which Koinon does not support yet (planned for v0.2); " +
	"use a server that accepts a static token or API key"

// Diagnosis is what a direct request to a server showed.
type Diagnosis struct {
	// Reachable says the server answered.
	Reachable bool
	// Status is the HTTP status, 0 when there was no answer.
	Status int
	// NeedsOAuth says the server wants an OAuth access token it expects the user to obtain: a 401
	// whose WWW-Authenticate carries an RFC 9728 resource_metadata URL, which the MCP authorization
	// specification requires of such servers.
	NeedsOAuth bool
	// Reason is a short, secret-free explanation of a failure; empty when the server answered 2xx.
	Reason string
}

// Diagnose sends one request to the server named by p, directly from this process (not through the
// gateway), and says what came back. It dials only addresses policy allows, follows no redirects and
// sends the credential to p.URL alone. The request is an MCP initialize for Streamable HTTP and a GET
// of the event stream for SSE; it never calls a tool.
func Diagnose(ctx context.Context, policy catalog.URLPolicy, p PreflightRequest) Diagnosis {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           policy.DialContext(&net.Dialer{Timeout: 5 * time.Second}),
			ResponseHeaderTimeout: 6 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var req *http.Request
	var err error
	if p.Transport == TransportSSE {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
		if req != nil {
			req.Header.Set("Accept", "text/event-stream")
		}
	} else {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"koinon-preflight","version":"0"}}}`
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, p.URL, strings.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
	}
	if err != nil {
		return Diagnosis{Reason: "the URL cannot be requested"}
	}
	if p.Value != "" && p.Header != "" {
		req.Header.Set(p.Header, p.Prefix+p.Value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Diagnosis{Reason: "could not connect: " + networkReason(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<16)
	d := Diagnosis{Reachable: true, Status: resp.StatusCode}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if strings.Contains(strings.ToLower(strings.Join(resp.Header.Values("Www-Authenticate"), " ")), "resource_metadata") {
			d.NeedsOAuth = true
			d.Reason = OAuthMessage
		} else if p.Value != "" {
			d.Reason = fmt.Sprintf("the server rejected the credential (HTTP %d)", resp.StatusCode)
		} else {
			d.Reason = fmt.Sprintf("the server asks for a credential (HTTP %d)", resp.StatusCode)
		}
	default:
		d.Reason = fmt.Sprintf("the server answered HTTP %d", resp.StatusCode)
	}
	return d
}

// networkReason names a connection failure without quoting addresses or anything a server sent.
func networkReason(err error) string {
	var inv *InvalidError
	var dns *net.DNSError
	switch {
	case errors.As(err, &inv):
		return "the address is not allowed by the egress policy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.As(err, &dns):
		return "the name does not resolve"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls"):
		return "TLS handshake failed"
	}
	return "network error"
}

// OAuthPreflight is a Preflight that refuses servers that need per-user OAuth. An unreachable server,
// or one that rejects the credential, is not refused here: it may be declared before it runs, and the
// prober reports its health.
func OAuthPreflight(policy catalog.URLPolicy) Preflight {
	return func(ctx context.Context, p PreflightRequest) error {
		if d := Diagnose(ctx, policy, p); d.NeedsOAuth {
			return &InvalidError{Msg: OAuthMessage}
		}
		return nil
	}
}
