// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
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
	// BadCertificate says the TLS handshake ended because the server's certificate does not verify
	// (an unknown CA, a name that does not match, an expired or otherwise invalid certificate), not
	// because the server is down. Reason then names the CA bundle field.
	BadCertificate bool
}

// CertificateMessage is what an admin reads when the server's certificate does not verify.
const CertificateMessage = "the server's TLS certificate does not verify; if it is signed by a private CA, " +
	"put that CA's PEM certificate in the server's CA bundle field (caBundle)"

// Diagnose sends one request to the server named by p, directly from this process, and says what came
// back. It is for the add-time OAuth check only, and it carries NO credential: the server is not
// known to the gateway yet, and a credential never leaves the gateway. A server that wants OAuth
// answers an anonymous request with a 401 naming its resource metadata; one that wants a static
// credential answers 401 without it. It dials only addresses policy allows and follows no redirects.
// The request is an MCP initialize for Streamable HTTP and a GET of the event stream for SSE; it never
// calls a tool.
func Diagnose(ctx context.Context, policy catalog.URLPolicy, p PreflightRequest) Diagnosis {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// The system roots plus the server's own bundle (only this server trusts it); TLS 1.2 at least.
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.CABundle != "" {
		pool, err := codehost.CertPool(p.CABundle)
		if err != nil {
			return Diagnosis{Reason: "the CA bundle is not usable"}
		}
		tlsConfig.RootCAs = pool
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           policy.DialContext(&net.Dialer{Timeout: 5 * time.Second}),
			TLSClientConfig:       tlsConfig,
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
	resp, err := client.Do(req)
	if err != nil {
		if certificateProblem(err) {
			return Diagnosis{BadCertificate: true, Reason: CertificateMessage}
		}
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
		} else {
			d.Reason = fmt.Sprintf("the server asks for a credential (HTTP %d)", resp.StatusCode)
		}
	default:
		d.Reason = fmt.Sprintf("the server answered HTTP %d", resp.StatusCode)
	}
	return d
}

// certificateProblem says err is the server's certificate failing verification. The typed errors are
// what crypto/tls returns (a *tls.CertificateVerificationError wraps them); a handshake that fails
// for another reason, or a server that is down, is not one.
func certificateProblem(err error) bool {
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &host) || errors.As(err, &invalid) || errors.As(err, &verify)
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

// OAuthPreflight is a Preflight that refuses servers that need per-user OAuth, and servers whose
// certificate does not verify. An unreachable server, or one that rejects the credential, is not refused here: it may be declared before it runs, and the
// prober reports its health.
func OAuthPreflight(policy catalog.URLPolicy) Preflight {
	return func(ctx context.Context, p PreflightRequest) error {
		d := Diagnose(ctx, policy, p)
		switch {
		case d.NeedsOAuth:
			return &InvalidError{Msg: OAuthMessage}
		case d.BadCertificate:
			// A certificate that does not verify is the admin's to fix now (a missing CA), and a
			// server saved this way would never connect. A server that is only down stays open.
			return &InvalidError{Msg: CertificateMessage}
		}
		return nil
	}
}
