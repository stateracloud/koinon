// SPDX-License-Identifier: Apache-2.0

// Package apiassembly builds the request path of koinon-api: the services, the admin REST routes
// and the HTTP handler, from what already exists outside it (ticket 1 of design-07-compose-api).
// Assemble opens no listener and starts no loop; the caller runs those from the Assembly.
package apiassembly

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/services"
)

// Outside is what the process owns and Assemble only uses.
type Outside struct {
	Pool *pgxpool.Pool
	// Kube is nil without Kubernetes access.
	Kube kubernetes.Interface
	// Keyring is nil when no key-encryption key is configured.
	Keyring *secretstore.Keyring
	// Binding is the install binding (ADR 0110); BindingOwner is the owner it was made with.
	Binding      *installbinding.Binding
	BindingOwner string
	// Ready answers /readyz.
	Ready http.HandlerFunc

	// The fields below are optional replacements for what Assemble builds from Settings. koinon-api
	// leaves them empty; the API suite sets them to bring a fake in the same place.

	// GitHub is the GitHub client, shared by connection checks and approved calls. Nil builds one
	// from Settings.GitHubAPIURL behind the egress policy.
	GitHub *github.Client
	// ModelChat is what the provider echo check sends its request through. Nil builds the gateway
	// chat from Settings.GatewayURL, or none when there is no gateway or no Kubernetes access.
	ModelChat modelcheck.Chat
	// UI is the web UI to serve. Nil serves web.Dist(); NoUI serves none.
	UI   fs.FS
	NoUI bool
	// Logger is where the services log. Nil uses slog.Default().
	Logger *slog.Logger
	// Services register more Connect services next to the platform's own.
	Services []func(*connect.Server)
}

// Prebuilt is what koinon-api's serve still builds itself and passes in. Interim: ticket 5 of
// design-07-compose-api deletes it.
type Prebuilt struct {
	Auth    *auth.Service
	Hub     *runwatch.Hub
	Metrics *api.Metrics
	// Notifier, DirectMessages and RunNow are nil without the DBOS runtime.
	Notifier       *durable.Notifier
	DirectMessages *services.DirectMessageQueue
	RunNow         *services.RunNowQueue
}

// Assemble builds the request path. It returns an *InputError for a Settings value no start can use.
func Assemble(_ context.Context, out Outside, set Settings, pre Prebuilt) (*Assembly, error) {
	if err := set.Validate(); err != nil {
		return nil, err
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	dom, err := buildDomain(out, set)
	if err != nil {
		return nil, err
	}
	return buildSurfaces(out, set, pre, dom)
}
