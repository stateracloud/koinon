// SPDX-License-Identifier: Apache-2.0

// Package providers is the code-host provider registry: the one place that lists the providers
// Koinon ships and maps their connector type IDs to implementations (ADR 0062). Everything else
// looks a provider up by the connector ID on the connection or approval request.
package providers

import (
	"context"
	"net"
	"sync"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
	"github.com/stateracloud/koinon/internal/connectors/github"
)

// GitHub is the connector type ID of the GitHub provider, for the few places whose wire
// contract is GitHub-specific (the CreateGitHubConnection RPC and the declarative GitHub
// connection).
const GitHub = github.HostID

// Gitea is the connector type ID of the Gitea provider, for the few places whose wire contract
// is Gitea-specific (the CreateGiteaConnection RPC and the declarative Gitea connection).
const Gitea = gitea.HostID

// New returns the registry of shipped providers. gh is the GitHub client, shared so connection
// checks and approved calls use one token cache; nil is a client for api.github.com. Gitea
// dials through the same egress policy check as gh (its DialContext), so one outbound rule
// covers every admin-supplied code-host URL.
func New(gh *github.Client) *codehost.Registry {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if gh != nil {
		dial = gh.DialContext()
	}
	return codehost.NewRegistry(github.NewHost(gh), gitea.NewHost(gitea.Options{DialContext: dial}))
}

var (
	defaultOnce sync.Once
	defaultReg  *codehost.Registry
)

// Default is a registry of the shipped providers for code that only needs to know which
// connector IDs are code hosts (grants, bindings, targets). Its clients make no calls unless
// opened.
func Default() *codehost.Registry {
	defaultOnce.Do(func() { defaultReg = New(nil) })
	return defaultReg
}
