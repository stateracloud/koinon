// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"time"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// A spent budget stops a run in two ways that race (ADR 0050): the ledger cancels the run and
// wakes the reconciler, which revokes the agent's key. A harness whose model request meets the
// revoked key gets 401 from the gateway (or 429 from a limit it enforces itself) before the
// cancel reaches this worker. Neither is the run's own fault, so a gateway refusal is checked
// against the same ledger: if a limit is reached the run ends as a budget stop, recorded like the
// ledger's cancel; if none is, the error stays what it says it is.

// budgetStop returns the cancellation that records err as a budget stop when err is a gateway
// refusal (401 or 429) and a limit of the run's scope is reached, and err itself otherwise. The
// limits are the ledger's: the agent's daily limit, the org's monthly cap and the platform
// ceiling, in their current windows. A ledger that cannot be read leaves err as it is.
func (r *runner) budgetStop(ctx context.Context, err error) error {
	var f *failure
	if !errors.As(err, &f) || !f.gateway {
		return err
	}
	st, berr := budget.Compute(ctx, r.q, r.run.OrgID, r.run.AgentID, money.FromCents(r.vcfg.Limits.SpendUSD.Cents()), time.Now())
	if berr != nil {
		r.log.Warn("checking the budget after a gateway refusal failed", "err", berr)
		return err
	}
	// The limit the ledger and the reconciler name (budget.Status.Pick): the widest one reached.
	l, found := st.Pick(time.Now())
	if !found {
		return err
	}
	r.log.Info("the gateway refused the model request and a limit is reached; stopping the run on the budget", "limit", l.Name, "refusal", f.reason)
	msg := l.StopMessage()
	extra := l.StopDetail()
	extra["reason"] = string(scheduler.ReasonSpendLimit)
	extra["message"] = msg
	return &canceled{reason: msg, class: classBudget, extra: extra}
}
