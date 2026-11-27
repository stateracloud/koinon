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

// whatEvent names what the run waits for, in the run event and the failure.
func whatEvent(forge bool) string {
	if forge {
		return "the agent's code-host credential"
	}
	return "the agent's sandbox identity"
}

// activeForgeGap says why koinon-reconciler will write no row for the agent, or "" when a pass can
// still write one. The reconciler writes the rows of active agents only, from the active version
// (forgegate.go). A run can be pinned to an older version than the active one (sandbox.go), and the
// agent can leave the active state after the run was claimed: then no wait brings a row. With forge
// false the run waits only for its actor (waitsForActor), which every active agent's row holds, so only
// an agent that is not active is a gap.
func (r *runner) activeForgeGap(ctx context.Context, forge bool) (string, error) {
	agent, err := r.q.GetAgent(ctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
	if err != nil {
		return "", err
	}
	if agent.Status != store.AgentStatusActive {
		if !forge {
			return "the agent is not active, so koinon-gateway does not serve its sandbox; activate the agent, then run the task again", nil
		}
		return "the agent is not active, so koinon-reconciler makes no code-host credential for it; activate the agent, then run the task again", nil
	}
	if !forge || !agent.ActiveVersionID.Valid || agent.ActiveVersionID == r.run.AgentVersionID {
		return "", nil
	}
	v, err := r.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: r.run.OrgID, ID: agent.ActiveVersionID})
	if err != nil {
		return "", err
	}
	cfg, err := versionConfig(v.Config)
	if err != nil {
		// koinon-reconciler cannot read it either: it stops the pass and writes no row and no cause.
		return "the agent's active version has a config that cannot be read (" + err.Error() + "), so koinon-reconciler makes no " +
			"code-host credential for the agent; fix the agent's config, then run the task again", nil
	}
	if _, ok, err := providers.Default().ReadGrant(cfg); !ok && err == nil {
		return "the run's agent version has a code-host read grant, but the agent's active version has none, " +
			"so koinon-reconciler makes no code-host credential for the agent; run the task again on the active version", nil
	}
	return "", nil
}

// waitsForActor says the run waits for the State revision that holds its actor (issue #1489): on Agent
// Substrate (ActorIdentityOutside) koinon-gateway checks the actor JWT on every listener, and refuses an
// actor that no applied revision holds. koinon-reconciler writes a row for every active agent there, also
// without a forge grant (forgeWant).
func (r *runner) waitsForActor() bool { return r.p.cfg.ActorIdentityOutside }

// waitForge waits until koinon-gateway serves the agent's forge credential, so the run's first clone
// through the egress listener is not refused with 503 credential_unavailable (#1086 part C2). On Agent
// Substrate it also waits until every live gateway copy holds the agent's actor (waitsForActor), so the
// harness's first model call is not refused with 403. The wait polls the database: no kubelet volume
// sync is on the path.
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
		warned   bool
		nudged   time.Time
		poll     = r.p.cfg.Poll
	)
	// A run whose version has no forge read grant expects no credential, whatever row the agent has:
	// a row left from an earlier version neither holds nor fails it. On Agent Substrate the run still
	// waits for the row of its actor.
	forge := r.expectsForge()
	if !forge && !r.waitsForActor() {
		return nil
	}
	what := "the agent's forge credential"
	if !forge {
		what = "the agent's sandbox (its actor)"
	}
	for {
		f, err := r.q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			gap, gerr := r.activeForgeGap(ctx, forge)
			if gerr == nil && gap != "" {
				return &failure{reason: gap}
			}
			if gerr != nil && ctx.Err() == nil && !warned {
				// Once: the loop polls again and fails at ForgeServeTimeout if the read keeps failing.
				warned = true
				r.log.Warn("reading the agent's active version failed", "err", gerr)
			}
			// The agent expects a credential and no pass wrote its row yet (a version with a new grant
			// that no pass has seen): wait for the pass, as for a row that is not served.
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if now().Sub(started) >= r.p.cfg.ForgeServeTimeout {
				return fmt.Errorf("check whether koinon-gateway serves %s: %w", what, err)
			}
			r.log.Warn("checking whether koinon-gateway serves "+what+" failed", "err", err)
			if !sleep(ctx, r.p.cfg.Poll) {
				return ctx.Err()
			}
			continue
		case f.Failure != "" && forge:
			// A run that expects no credential ignores the failure of the active version's grant: it waits
			// for the revision of its actor only, which the failure row carries on Agent Substrate.
			return &failure{reason: f.Failure + "; check the agent's code-host connection and the koinon-reconciler log, then run the task again"}
		case f.ServedAt.Valid:
			if forge && r.waitsForActor() {
				// On Agent Substrate the reconciler writes a row for every active agent, with or without a
				// credential: a served row of an active version without a grant holds no credential for this run.
				gap, gerr := r.activeForgeGap(ctx, true)
				if gerr != nil {
					return gerr
				}
				if gap != "" {
					return &failure{reason: gap}
				}
			}
			if recorded {
				r.log.Info("koinon-gateway serves "+what+" now", "waited", now().Sub(started))
			}
			return nil
		}
		if !recorded {
			recorded = true
			r.log.Info("waiting for koinon-gateway to serve " + what)
			if err := r.own.Record(ctx, forgeWaitKind, map[string]any{
				"reason": "waiting for koinon-gateway to serve " + whatEvent(forge),
			}); err != nil {
				return err
			}
		}
		if waited := now().Sub(started); waited >= r.p.cfg.ForgeServeTimeout {
			return &failure{reason: fmt.Sprintf("koinon-gateway did not serve %s within %s. "+
				"Check the koinon-reconciler and koinon-gateway logs for why it is not served, then run the task again", whatEvent(forge), r.p.cfg.ForgeServeTimeout)}
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
