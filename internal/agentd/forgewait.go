// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/db/store"
)

// DefaultForgeServeTimeout bounds the wait for koinon-gateway to serve an agent's forge credential. A
// new credential's file takes up to the reconciler's key settle (90 s by default) to reach the gateway
// pod, then a pass stores the State and every live copy applies it; five minutes is several times that.
const DefaultForgeServeTimeout = 5 * time.Minute

// forgeWaitKind is the run event recorded when a run starts waiting for its forge credential.
const forgeWaitKind = "run.waiting_for_forge"

// expectsForge is the rule koinon-reconciler also writes the rows by (codehost.Registry.ReadGrant): the
// run's agent version has a forge read grant. A grant that cannot be read counts as one: the reconciler
// then writes the cause.
func (r *runner) expectsForge() bool {
	_, ok, err := providers.Default().ReadGrant(r.vcfg)
	return ok || err != nil
}

// activeExpectsForge applies the same rule to the agent's active version, the one koinon-reconciler
// renders the forge routes from. A run can be pinned to an older version than the active one
// (sandbox.go): when only the pinned version has a grant, the reconciler writes no row, and no wait
// brings one.
func (r *runner) activeExpectsForge(ctx context.Context) (bool, error) {
	agent, err := r.q.GetAgent(ctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
	if err != nil {
		return false, err
	}
	if !agent.ActiveVersionID.Valid || agent.ActiveVersionID == r.run.AgentVersionID {
		return true, nil
	}
	v, err := r.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: r.run.OrgID, ID: agent.ActiveVersionID})
	if err != nil {
		return false, err
	}
	cfg, err := versionConfig(v.Config)
	if err != nil {
		return true, nil // the reconciler writes the cause
	}
	_, ok, err := providers.Default().ReadGrant(cfg)
	return ok || err != nil, nil
}

// waitForge waits until koinon-gateway serves the agent's forge credential, so the run's first clone
// through the egress listener is not refused with 503 credential_unavailable (#1086 part C2).
// koinon-reconciler is the only component that knows: it records in gateway_agent_forge, for each
// agent with a forge read grant, the State revision that holds the credential, and when every live
// copy acked it. No row, for an agent whose version has no forge read grant, means the agent expects no
// credential, and the run starts at once; with a grant, the run waits for a pass to write the row. A row with
// a failure fails the run with that cause: the reconciler cannot make the credential, and no wait
// brings it. While the run waits it records run.waiting_for_forge once and NOTIFYs the reconciler every
// ReconcileNudge. After ForgeServeTimeout the run fails.
func (r *runner) waitForge(ctx context.Context) error {
	now := r.p.cfg.Now
	started := now()
	var (
		recorded bool
		nudged   time.Time
		poll     = r.p.cfg.Poll
	)
	// A run whose version has no forge read grant expects no credential, whatever row the agent has:
	// a row left from an earlier version neither holds nor fails it.
	if !r.expectsForge() {
		return nil
	}
	for {
		f, err := r.q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			active, aerr := r.activeExpectsForge(ctx)
			if aerr == nil && !active {
				return &failure{reason: "the run's agent version has a code-host read grant, but the agent's active version has none, " +
					"so koinon-reconciler makes no code-host credential for the agent; run the task again on the active version"}
			}
			if aerr != nil && ctx.Err() == nil {
				r.log.Warn("reading the agent's active version failed", "err", aerr)
			}
			// The agent expects a credential and no pass wrote its row yet (a version with a new grant
			// that no pass has seen): wait for the pass, as for a row that is not served.
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if now().Sub(started) >= r.p.cfg.ForgeServeTimeout {
				return fmt.Errorf("check whether koinon-gateway serves the agent's forge credential: %w", err)
			}
			r.log.Warn("checking whether koinon-gateway serves the agent's forge credential failed", "err", err)
			if !sleep(ctx, r.p.cfg.Poll) {
				return ctx.Err()
			}
			continue
		case f.Failure != "":
			return &failure{reason: f.Failure + "; check the agent's code-host connection and the koinon-reconciler log, then run the task again"}
		case f.ServedAt.Valid:
			if recorded {
				r.log.Info("koinon-gateway serves the agent's forge credential now", "waited", now().Sub(started))
			}
			return nil
		}
		if !recorded {
			recorded = true
			r.log.Info("waiting for koinon-gateway to serve the agent's forge credential")
			if err := r.own.Record(ctx, forgeWaitKind, map[string]any{
				"reason": "waiting for koinon-gateway to serve the agent's code-host credential",
			}); err != nil {
				return err
			}
		}
		if waited := now().Sub(started); waited >= r.p.cfg.ForgeServeTimeout {
			return &failure{reason: fmt.Sprintf("koinon-gateway did not serve the agent's code-host credential within %s. "+
				"Check the koinon-reconciler and koinon-gateway logs for why it is not served, then run the task again", r.p.cfg.ForgeServeTimeout)}
		}
		if now().Sub(nudged) >= r.p.cfg.ReconcileNudge {
			nudged = now()
			if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil && ctx.Err() == nil {
				r.log.Warn("notifying the reconciler failed", "err", err)
			}
		}
		if !sleep(ctx, poll) {
			return ctx.Err()
		}
		// Back off to a few seconds: a new credential takes a minute or more to reach the gateway.
		poll = min(2*poll, max(r.p.cfg.Poll, 5*time.Second))
	}
}
