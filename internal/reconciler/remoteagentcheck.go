// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/safego"
)

// The remote agent health check (#1129, ADR 0123 decision 11). Every probe interval the leader reads the
// agent card of each enabled remote agent and records the result (remoteagents.Service.RecordCheck):
//
//   - the card is the pin: the agent is healthy;
//   - the card is not the pin: the card is stored as a finding and the agent is unhealthy at once. The
//     pin does not move; only an admin accepts a card;
//   - the read failed: after three failures in a row the agent is unhealthy.
//
// The check runs while this replica leads, so one replica reads each card. The read uses the same HTTP
// client, egress policy and credential rules as a call. A reason is a fixed text: it never carries data
// from the agent.

const (
	remoteAgentCheckTimeout  = 30 * time.Second
	remoteAgentCheckParallel = 8
)

// RunRemoteAgentChecker checks until ctx ends.
func (r *Reconciler) RunRemoteAgentChecker(ctx context.Context) {
	t := time.NewTicker(r.cfg.MCPProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safego.Do(r.log, "remote-agent-check", func() { r.CheckRemoteAgents(ctx) })
		}
	}
}

// CheckRemoteAgents checks every enabled remote agent of every org once.
func (r *Reconciler) CheckRemoteAgents(ctx context.Context) {
	orgs, err := r.remoteAgents.ListOrgsToCheck(ctx)
	if err != nil {
		r.log.Warn("remote agent check: listing orgs failed", "err", err)
		return
	}
	sem := make(chan struct{}, remoteAgentCheckParallel)
	var wg sync.WaitGroup
	for _, org := range orgs {
		rows, err := r.remoteAgents.ListCheckable(ctx, org)
		if err != nil {
			r.log.Warn("remote agent check: listing agents failed", "org", org.String(), "err", err)
			continue
		}
		for _, row := range rows {
			wg.Add(1)
			sem <- struct{}{}
			safego.Go(ctx, r.log, "remote-agent-check", func(ctx context.Context) {
				defer wg.Done()
				defer func() { <-sem }()
				cctx, cancel := context.WithTimeout(ctx, remoteAgentCheckTimeout)
				defer cancel()
				if _, err := r.remoteAgents.Check(cctx, row); err != nil && ctx.Err() == nil {
					r.log.Warn("remote agent check failed", "org", row.OrgID.String(), "agent", row.ID.String(), "err", err)
				}
			})
		}
	}
	wg.Wait()
}
