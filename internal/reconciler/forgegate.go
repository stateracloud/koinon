// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// The forge gate (#1086 part C2). An agent with a forge read grant clones through koinon-gateway's
// egress listener, and the gateway attaches the agent's sealed code-host token there. A new credential
// reaches the gateway only after its file settled in the gateway pod (gatewayplan settleForge), and then
// every live copy must apply the State revision that holds it. Until then a clone fails with 503
// credential_unavailable. So the reconciler records, per agent, the revision that serves the credential,
// and koinon-agentd starts the harness only when every live copy acked it (agentd waitForge). The
// sandbox work does not wait: the sandbox is made while the file settles.

// forgeFailureMax bounds the stored cause. The cause is an error of the reconciler, never a token.
const forgeFailureMax = 1000

// forgeRow is what gateway_agent_forge holds for one agent.
type forgeRow struct {
	// rev is the first State revision that holds the credential; 0 while no stored revision holds it.
	rev     int64
	failure string
	served  bool
}

// noteForge records, for each forge credential that revision rev holds, the first revision that held
// it. A credential the map already has keeps its earlier revision; a credential rev does not hold is
// dropped, so when it comes back (a new grant, after its file settled) it waits for a new revision.
// After a start the first revision seen counts: an agent waits once for the copies to ack it.
func (r *Reconciler) noteForge(creds []route.Credential, rev int64) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	next := make(map[string]int64, len(creds))
	for _, c := range creds {
		if old, ok := r.forgeRevs[c.Name]; ok && old <= rev {
			next[c.Name] = old
		} else {
			next[c.Name] = rev
		}
	}
	r.forgeRevs = next
}

// forgeRev is the first stored revision that holds the forge credential name, if one does.
func (r *Reconciler) forgeRev(name string) (int64, bool) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	rev, ok := r.forgeRevs[name]
	return rev, ok
}

// forgeWant is what the active agent w expects of its forge credential, and false when it expects
// none: it has no forge read grant, or the routes name no credential (no gateway key source). The rule
// is agentEgress's, which is the one that makes the routes.
func (r *Reconciler) forgeWant(w *work) (forgeRow, bool) {
	if w.agent.Status != store.AgentStatusActive {
		return forgeRow{}, false
	}
	if w.forgeErr != nil {
		msg := "koinon-reconciler has no forge credential for the agent: " + w.forgeErr.Error()
		if len(msg) > forgeFailureMax {
			msg = msg[:forgeFailureMax]
		}
		return forgeRow{failure: msg}, true
	}
	if w.forge == nil {
		return forgeRow{}, false
	}
	rev, ok := r.forgeRev(w.forge.Name)
	if !ok {
		return forgeRow{}, true
	}
	return forgeRow{rev: rev}, true
}

// recordForge writes what each agent of the pass expects of its forge credential (gateway_agent_forge)
// and marks the rows whose revision every live koinon-gateway copy acked. When a revision is not acked
// yet it waits for the acks (awaitGatewayAcked, bounded), as the key gate does. An agent that no longer
// expects a credential loses its row. Only changes go to the database.
func (r *Reconciler) recordForge(ctx context.Context, items []*work) error {
	first := r.forgeRows == nil
	if first {
		r.forgeRows = map[agentKey]forgeRow{}
	}
	want := map[agentKey]forgeRow{}
	drop := map[pgtype.UUID][]pgtype.UUID{}
	var maxRev int64
	for _, w := range items {
		k := agentKey{w.org.ID, w.agent.ID}
		row, ok := r.forgeWant(w)
		if !ok {
			if _, had := r.forgeRows[k]; had || first {
				drop[w.org.ID] = append(drop[w.org.ID], w.agent.ID)
			}
			continue
		}
		if old, had := r.forgeRows[k]; had && old.rev == row.rev && old.failure == row.failure {
			row.served = old.served
		}
		if row.rev > 0 && !row.served {
			maxRev = max(maxRev, row.rev)
		}
		want[k] = row
	}
	var gate *keyGate
	if maxRev > 0 {
		if !r.newKeyGate(ctx, nil).ackedRev(ctx, maxRev) {
			r.awaitGatewayAcked(ctx, maxRev)
		}
		gate = r.newKeyGate(ctx, nil)
	}
	var errs []error
	for k, row := range want {
		if row.rev > 0 && !row.served {
			row.served = gate.ackedRev(ctx, row.rev)
		}
		old, had := r.forgeRows[k]
		if !had || old.rev != row.rev || old.failure != row.failure {
			if err := r.q.SetGatewayAgentForge(ctx, store.SetGatewayAgentForgeParams{
				OrgID: k.org, AgentID: k.agent, StateRevision: pgtype.Int8{Int64: row.rev, Valid: row.rev > 0}, Failure: row.failure,
			}); err != nil {
				errs = append(errs, err)
				delete(r.forgeRows, k)
				continue
			}
			old = forgeRow{rev: row.rev, failure: row.failure}
		}
		if row.served && !old.served {
			if err := r.q.MarkGatewayAgentForgeServed(ctx, store.MarkGatewayAgentForgeServedParams{
				OrgID: k.org, AgentID: k.agent, StateRevision: pgtype.Int8{Int64: row.rev, Valid: true},
			}); err != nil {
				errs = append(errs, err)
				row.served = false
			}
		}
		r.forgeRows[k] = row
	}
	for org, ids := range drop {
		if err := r.q.DeleteGatewayAgentForge(ctx, store.DeleteGatewayAgentForgeParams{OrgID: org, AgentIds: ids}); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range ids {
			delete(r.forgeRows, agentKey{org, id})
		}
	}
	// The rows of agents that left the pass stay in the database (an agent whose prepare failed keeps
	// what it had); the next pass that has them writes them again if they changed.
	live := make(map[agentKey]bool, len(items))
	for _, w := range items {
		live[agentKey{w.org.ID, w.agent.ID}] = true
	}
	for k := range r.forgeRows {
		if !live[k] {
			delete(r.forgeRows, k)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("reconciler: record the forge credential state: %w", errors.Join(errs...))
	}
	return nil
}
