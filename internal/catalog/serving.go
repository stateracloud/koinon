// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Serving states of a provider (issue #17).
const (
	// ServingNone: the provider is disabled, removed or held off, so it is not served by design.
	ServingNone = ""
	// ServingSaved: saved, not served yet. Not a failure: the reconciler applies it on its next pass
	// and a new key reaches the gateway pod after about 3 minutes.
	ServingSaved = "saved"
	// ServingServing: the gateway serves every enabled model of the provider.
	ServingServing = "serving"
	// ServingFailed: the gateway does not serve the provider and Reason says why.
	ServingFailed = "failed"
)

// Reasons a provider the gateway does not serve is failed. The reconciler records the last two.
const (
	ReasonNoKey          = "no key"
	ReasonGatewayRejects = "gateway rejected config"
	ReasonUnreachable    = "upstream unreachable"
	ReasonUnknown        = "unknown"
)

// SavedGrace is how long a provider may stay saved before it counts as failed. A new key reaches the
// pod in about 3 minutes; a pass that stops before it records a status (a key store error, a database
// error) would otherwise leave the provider saved for ever.
const SavedGrace = 10 * time.Minute

// Serving is whether agentgateway serves a provider, derived from the models the reconciler
// recorded as served (gateway_served_models) and the failure it recorded for the last gateway
// apply (gateway_provider_failures). The API never calls the gateway.
type Serving struct {
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Detail is what the gateway or the reconciler said, for an admin; never a key.
	Detail string `json:"detail,omitempty"`
}

// servingOf decides the state of p from the recorded facts.
func servingOf(p Provider, f store.ListProviderServingRow) Serving {
	if p.DisabledReason != "" || p.Removed || p.AdminDisabled != nil {
		return Serving{State: ServingNone}
	}
	if p.CredentialLostAt != nil {
		return Serving{State: ServingFailed, Reason: ReasonNoKey, Detail: "the API key was lost and must be entered again"}
	}
	if f.Models == 0 {
		return Serving{State: ServingNone} // nothing to serve yet: no state, so the page does not wait
	}
	if f.Served >= f.Models {
		return Serving{State: ServingServing}
	}
	if f.FailureReason != "" {
		reason := f.FailureReason
		switch reason {
		case ReasonGatewayRejects, ReasonUnreachable, ReasonNoKey:
		default:
			reason = ReasonUnknown
		}
		return Serving{State: ServingFailed, Reason: reason, Detail: f.FailureDetail}
	}
	// The age comes from the database clock (saved_age_seconds), the one that stamped updated_at.
	if age := time.Duration(f.SavedAgeSeconds * float64(time.Second)); age > SavedGrace {
		return Serving{State: ServingFailed, Reason: ReasonUnknown, Detail: unservedDetail(age)}
	}
	return Serving{State: ServingSaved}
}

// withServing fills the serving status of the org's providers.
func (s *Service) withServing(ctx context.Context, org pgtype.UUID, list []Provider) error {
	rows, err := store.New(s.pool).ListProviderServing(ctx, org)
	if err != nil {
		return err
	}
	facts := make(map[string]store.ListProviderServingRow, len(rows))
	for _, r := range rows {
		facts[r.ProviderID.String()] = r
	}
	for i := range list {
		list[i].Serving = servingOf(list[i], facts[list[i].ID])
	}
	return nil
}

// unservedDetail says how long a provider has been saved without being served. Past an hour the
// minute count is no use, so it gives hours and days.
func unservedDetail(age time.Duration) string {
	switch {
	case age < time.Hour:
		return fmt.Sprintf("not served %d min after saving; see the reconciler log", int(age/time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("not served %d h after saving; see the reconciler log", int(age/time.Hour))
	default:
		return fmt.Sprintf("not served %d days after saving; see the reconciler log", int(age/(24*time.Hour)))
	}
}
