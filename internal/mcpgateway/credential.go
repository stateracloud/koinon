// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/mcpcredfile"
	"github.com/stateracloud/koinon/internal/secretstore"
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
	// RecipientKeyID is the ID of the gateway key that opened a sealed credential (ADR 0097 decision 12). It
	// is empty for a plaintext credential. It is a name, never key material.
	RecipientKeyID string
}

// OpenError is the failure to read a sealed credential. The call is refused and nothing goes upstream. Reason
// is one of the fixed reasons of [gatewaykeys.Metrics]; KeyID is the key ID the envelope names, when it has
// one. The text carries no byte of the envelope or the credential.
type OpenError struct {
	Reason string
	KeyID  string
	Err    error
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("mcpgateway: the sealed credential cannot be opened (%s)", e.Reason)
}

func (e *OpenError) Unwrap() error { return e.Err }

// CredentialSource resolves the credential for a call. It runs on every upstream request, so a
// rotated Secret file or a refreshed token applies without a restart.
type CredentialSource interface {
	Credential(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) (Credential, error)
}

// StaticCredentials is the CredentialSource of ADR 0057 item 5: the header, prefix and file of the
// target.
//
// A file named mcpv2-... holds a v2 envelope (ADR 0097 decision 12): the gateway opens it with Keys. With no
// Keys, or with a key that does not fit, the read fails closed (an [*OpenError]). Any other file holds the
// plaintext of a v1 credential, as before.
type StaticCredentials struct {
	// Keys are the gateway's recipient keys. Nil means no sealed credential can be opened.
	Keys *secretstore.RecipientKeyring
	// OnOpenFailure counts a refused sealed credential by its fixed reason. It may be nil. It runs on every
	// refused read, and one call can read the credential more than once.
	OnOpenFailure func(reason string)
}

// Credential implements CredentialSource.
func (c StaticCredentials) Credential(_ context.Context, t gateway.MCPTarget, _ gateway.AgentRef) (Credential, error) {
	if t.CredentialHeader == "" || t.CredentialFile == "" {
		return Credential{Kind: CredentialNone}, nil
	}
	b, err := os.ReadFile(t.CredentialFile)
	if err != nil {
		// The error names the file, never its content.
		return Credential{}, err
	}
	if base := filepath.Base(t.CredentialFile); mcpcredfile.IsSealed(base) {
		plain, keyID, err := c.open(base, b)
		if err != nil {
			var oe *OpenError
			if c.OnOpenFailure != nil && errors.As(err, &oe) {
				c.OnOpenFailure(oe.Reason)
			}
			return Credential{}, err
		}
		defer clear(plain)
		return Credential{
			Kind: CredentialStatic, Header: t.CredentialHeader,
			Value: t.CredentialPrefix + strings.TrimRight(string(plain), "\r\n"), RecipientKeyID: keyID,
		}, nil
	}
	return Credential{
		Kind: CredentialStatic, Header: t.CredentialHeader,
		Value: t.CredentialPrefix + strings.TrimRight(string(b), "\r\n"),
	}, nil
}

// open decrypts the content of a v2 credential file. The binding comes from the file name, and the envelope
// is authenticated against it: a file copied to another server, org or column fails to open.
func (c StaticCredentials) open(base string, content []byte) (plain []byte, keyID string, err error) {
	var env secretstore.EnvelopeV2
	if err := json.Unmarshal(content, &env); err != nil || env.Format != secretstore.FormatV2 {
		return nil, "", &OpenError{Reason: gatewaykeys.ReasonFormat, Err: secretstore.ErrUnsupportedFormat}
	}
	// The key ID comes from the file, so it is checked before it reaches a log row: a bad one is dropped.
	if !secretstore.ValidRecipientKeyID(env.RecipientKeyID) {
		env.RecipientKeyID = ""
	}
	org, server, ok := mcpcredfile.ParseSealed(base)
	if !ok {
		return nil, env.RecipientKeyID, &OpenError{Reason: gatewaykeys.ReasonFormat, KeyID: env.RecipientKeyID, Err: secretstore.ErrUnsupportedFormat}
	}
	if c.Keys == nil {
		return nil, env.RecipientKeyID, &OpenError{Reason: gatewaykeys.ReasonUnknownKey, KeyID: env.RecipientKeyID, Err: secretstore.ErrUnknownRecipientKey}
	}
	plain, err = c.Keys.Open(mcpcredfile.Binding(org, server), env)
	if err != nil {
		reason := gatewaykeys.ReasonOpen
		switch {
		case errors.Is(err, secretstore.ErrUnknownRecipientKey):
			reason = gatewaykeys.ReasonUnknownKey
		case errors.Is(err, secretstore.ErrUnsupportedFormat):
			reason = gatewaykeys.ReasonFormat
		}
		return nil, env.RecipientKeyID, &OpenError{Reason: reason, KeyID: env.RecipientKeyID, Err: err}
	}
	return plain, env.RecipientKeyID, nil
}

// credentialTransport sets the credential on each upstream request. The SDK builds the request, so
// no header of the agent's request is ever on it.
//
// A platform target (Org "": Koinon's own tool servers) also gets gateway.AgentHeader, the agent the key
// check found (ADR 0097 decision 5, exit criterion 7). Those servers trust it because only the gateway
// reaches them. An org's server never gets it: it trusts nothing of Koinon's and needs no agent ID.
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
	// A RoundTripper must not change the SDK's request: every header goes on a clone.
	req = req.Clone(req.Context())
	if t.target.Org == "" {
		req.Header.Set(gateway.AgentHeader, t.agent.String())
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
	req.Header.Set(cred.Header, cred.Value)
	return t.base.RoundTrip(req)
}

// credentialSafeURL says a credential may travel to raw (see gateway.CredentialSafeURL).
func credentialSafeURL(raw string) bool { return gateway.CredentialSafeURL(raw) }
