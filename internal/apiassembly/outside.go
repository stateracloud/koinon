// SPDX-License-Identifier: Apache-2.0

// Package apiassembly builds the request path of koinon-api: the services, the admin REST routes
// and the HTTP handler, from what already exists outside it (ticket 1 of design-07-compose-api).
// Assemble opens no listener and starts no loop; the caller runs those from the Assembly.
package apiassembly

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/installbinding"
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
	dom, err := buildDomain(out, set)
	if err != nil {
		return nil, err
	}
	return buildSurfaces(out, set, pre, dom)
}
