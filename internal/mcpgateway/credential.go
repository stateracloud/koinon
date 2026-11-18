// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Credential kinds, as the span and the audit row name them. They are fixed values, never a token.
const (
	CredentialNone   = "none"
	CredentialStatic = "static"
	// CredentialError is the kind of a call whose credential could not be read.
	CredentialError = "error"
)

// Credential is what the gateway sends to one target for one agent. It exists only between the
// gateway and the target: an agent never sees it, and no span, log line or audit row carries Value.
type Credential struct {
	// Kind names the source: CredentialNone, CredentialStatic, and in later slices the MCP sign-in
	// token (ADR 0096) and the passport (ADR 0073).
	Kind string
	// Header and Value are set on every request to the target. Both are empty for CredentialNone.
	Header string
	Value  string
}

// CredentialSource resolves the credential for a call. It runs on every upstream request, so a
// rotated Secret file or a refreshed token applies without a restart.
type CredentialSource interface {
	Credential(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) (Credential, error)
}

// StaticCredentials is the CredentialSource of ADR 0057 item 5: the header, prefix and file of the
// target.
type StaticCredentials struct{}

// Credential implements CredentialSource.
func (StaticCredentials) Credential(_ context.Context, t gateway.MCPTarget, _ gateway.AgentRef) (Credential, error) {
	if t.CredentialHeader == "" || t.CredentialFile == "" {
		return Credential{Kind: CredentialNone}, nil
	}
	b, err := os.ReadFile(t.CredentialFile)
	if err != nil {
		// The error names the file, never its content.
		return Credential{}, err
	}
	return Credential{
		Kind: CredentialStatic, Header: t.CredentialHeader,
		Value: t.CredentialPrefix + strings.TrimRight(string(b), "\r\n"),
	}, nil
}

// credentialTransport sets the credential on each upstream request. The SDK builds the request, so
// no header of the agent's request is ever on it.
type credentialTransport struct {
	base   http.RoundTripper
	target gateway.MCPTarget
	agent  gateway.AgentRef
	src    CredentialSource
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred, err := t.src.Credential(req.Context(), t.target, t.agent)
	if err != nil {
		return nil, err
	}
	if cred.Header == "" {
		return t.base.RoundTrip(req)
	}
	// Never send the credential to another host than the target's, even after a redirect, and
	// never in clear text.
	if u, perr := url.Parse(t.target.URL); perr != nil || u.Host != req.URL.Host {
		return nil, errors.New("mcpgateway: refused to send a credential to another host")
	}
	if !credentialSafeURL(req.URL.String()) {
		return nil, errors.New("mcpgateway: refused to send a credential over a URL that is not https")
	}
	req = req.Clone(req.Context())
	req.Header.Set(cred.Header, cred.Value)
	return t.base.RoundTrip(req)
}

// credentialSafeURL says a credential may travel to raw. It must be https. A loopback address is
// the one exception: its traffic does not leave the machine (a sidecar, a test).
func credentialSafeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
