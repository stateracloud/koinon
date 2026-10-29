// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
)

// Who refused a model request (ADR 0030, amendment 2026-10-29).
//
// OpenCode 2.2.0 reports a provider's 401 and 403 alike as the ACP error -32000 "Authentication
// required: provider authentication required", with no status. Read literally it says the agent's
// gateway key failed; a run whose model provider answered 403 (OpenRouter's moderation flag, a
// guardrail) was therefore failed as a gateway-key fault and sent its people to debug keys that
// were fine. The gateway knows better: its access log, which the spend ledger records, says whether
// it called the provider and what status the provider answered. agentd asks the ledger.
//
//   - a provider 403: policy, "the provider refused the request";
//   - a provider 401: the provider rejected the key Koinon holds for it, a fault of the provider
//     record and not of the agent's gateway key;
//   - no record of either: the gateway refused the key itself (a bad or revoked key is answered
//     before any provider is called and has no agent to attribute the record to), the reading the
//     error text gave before.

const (
	// defaultGatewayRecordWait is how long to wait for the access-log record: agentgateway exports
	// in batches, so a record reaches the ledger some seconds after the request.
	defaultGatewayRecordWait = 20 * time.Second
	gatewayRecordPoll        = 250 * time.Millisecond
	// recordClockSlack is how far before the turn's start a record may be stamped: the gateway's
	// clock is not agentd's.
	recordClockSlack = 5 * time.Second
)

// providerRefusal is the gateway's record of a model request its provider refused.
type providerRefusal struct {
	status int
	// detail is the excerpt of the provider's error (package errbody), empty when the gateway
	// logged none.
	detail       string
	model        string
	providerType string
}

// authRefusals are the statuses that harnesses report as an authentication failure.
var authRefusals = []int32{401, 403}

// providerRefusal improves the failure of a turn whose error the harness worded as an
// authentication failure, from the gateway's record in the spend ledger. Any other error, and an
// authentication failure without a record, stay as they are.
func (r *runner) providerRefusal(ctx context.Context, err error, since time.Time) error {
	var f *failure
	if !errors.As(err, &f) || !f.authLike {
		return err
	}
	ref, found := r.awaitRefusal(ctx, since.Add(-recordClockSlack))
	if stop := r.stopped(ctx); stop != nil {
		return stop // the run was canceled or fenced while the record was awaited: that decides, as in harnessErr
	}
	if !found {
		return err
	}
	if m, merr := prices.Lookup(ctx, r.q, r.run.OrgID, ref.model); merr == nil {
		ref.providerType = m.ProviderType
	}
	r.log.Info("the gateway's record says the model provider refused the request", "status", ref.status, "model", ref.model, "provider_type", ref.providerType)
	return refusedByProvider(f, ref)
}

// awaitRefusal polls the ledger for the run's last provider 401 or 403 since the turn started,
// until the record arrives or the wait ends.
func (r *runner) awaitRefusal(ctx context.Context, since time.Time) (providerRefusal, bool) {
	wait := r.p.cfg.GatewayRecordWait
	deadline := time.Now().Add(max(wait, 0))
	for {
		row, err := r.q.GetLastUpstreamRefusal(ctx, store.GetLastUpstreamRefusalParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}, Statuses: authRefusals,
		})
		switch {
		case err == nil:
			return providerRefusal{status: int(row.Status), detail: row.Detail, model: row.Model}, true
		case !errors.Is(err, pgx.ErrNoRows):
			r.log.Warn("reading the gateway's record of the refused request failed", "err", err)
			return providerRefusal{}, false
		}
		if !time.Now().Before(deadline) {
			return providerRefusal{}, false
		}
		select {
		case <-ctx.Done():
			return providerRefusal{}, false
		case <-r.fenced:
			return providerRefusal{}, false
		case <-time.After(min(gatewayRecordPoll, time.Until(deadline))):
		}
	}
}

// refusedByProvider rewrites an authentication failure into what the gateway's record shows.
func refusedByProvider(f *failure, ref providerRefusal) *failure {
	out := *f
	out.gateway = false // not the gateway's key or limit: the budget check must not take it
	out.authLike = false
	from := "the model provider"
	if ref.providerType != "" {
		from = "the model provider (" + ref.providerType + ")"
	}
	detail := ""
	if ref.detail != "" {
		detail = ": " + ref.detail
	}
	switch ref.status {
	case 403:
		out.class = classPolicy
		out.reason = fmt.Sprintf("%s refused the request (HTTP 403)%s [%s]", capitalize(from), detail, f.reason)
	default: // 401
		out.class = classFatal
		out.reason = fmt.Sprintf("%s rejected the API key Koinon holds for it (HTTP 401)%s; check the provider's key in the catalog [%s]", capitalize(from), detail, f.reason)
	}
	return &out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
