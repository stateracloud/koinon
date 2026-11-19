// SPDX-License-Identifier: Apache-2.0

package remoteagents

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// FetchRequest is one read of an agent card.
type FetchRequest struct {
	URL string
	// Header, Prefix and Secret are the credential sent with the read. All empty means none.
	Header, Prefix, Secret string
	CABundle               string
	InsecureSkipVerify     bool
}

// CardFetcher reads and canonicalizes the agent card of a remote agent. The default is a2aout.FetchCard,
// with the same HTTP client, the same egress policy and the same credential rules as a call.
type CardFetcher func(ctx context.Context, r FetchRequest, policy catalog.URLPolicy) (*a2aout.Card, error)

// memCredentials serves one credential from memory, so the secret is never written to a file.
type memCredentials struct{ header, value string }

func (m memCredentials) Credential(context.Context, gateway.MCPTarget, gateway.AgentRef) (mcpgateway.Credential, error) {
	if m.header == "" {
		return mcpgateway.Credential{Kind: mcpgateway.CredentialNone}, nil
	}
	return mcpgateway.Credential{Kind: mcpgateway.CredentialStatic, Header: m.header, Value: m.value}, nil
}

func (s *Service) defaultFetch(ctx context.Context, r FetchRequest, policy catalog.URLPolicy) (*a2aout.Card, error) {
	t := gateway.MCPTarget{
		Name: "remote-agent", URL: r.URL, CredentialHeader: r.Header, CredentialPrefix: r.Prefix, InsecureSkipVerify: r.InsecureSkipVerify,
	}
	if r.CABundle != "" {
		// The HTTP client reads the bundle from a file. It holds certificates only, and goes at once.
		dir, err := os.MkdirTemp("", "koinon-remote-agent-ca-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		t.CAFile = filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(t.CAFile, []byte(r.CABundle), 0o600); err != nil {
			return nil, err
		}
	}
	value := ""
	if r.Header != "" {
		value = r.Prefix + r.Secret
	}
	return a2aout.FetchCard(ctx, a2aout.Options{
		Target: t, Policy: policy, Credentials: memCredentials{header: r.Header, value: value},
	})
}

// reasonOf maps the error of a card read to a fixed text. The text never carries data from the agent.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, a2aout.ErrEgressDenied):
		return ReasonEgressDenied
	case errors.Is(err, a2aout.ErrCardHost):
		return ReasonCardHost
	case errors.Is(err, a2aout.ErrCard):
		return ReasonCardInvalid
	}
	return ReasonUnreachable
}
