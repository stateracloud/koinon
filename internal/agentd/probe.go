// SPDX-License-Identifier: Apache-2.0

package agentd

// Storage probes for keys new-repositories and keys forget (#285).
//
// The keys command holds the KEKs but no bucket credentials, and koinon-agentd the reverse
// (ADR 0032), so only agentd can see that the prefix of the epoch a rotation would start already
// holds a repository the database has not recorded (#262). The command writes a probe request
// (home_repository_probes: the agent and the candidate epoch) and waits; agentd claims it, lists
// the agent's epoch folders with the same listing a resync uses (HomeStore.HeldEpochs: any
// object under r<N>/ means the prefix is taken, whatever its password), and answers in the row:
// free, taken or error, with a message that carries no secret. A claim is a lease, so a replica
// that dies leaves the probe to another; the command stops waiting at its own timeout and refuses
// the rotation then (fail closed), so an expired probe is never claimed.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/safego"
)

// The outcomes a probe is answered with (home_repository_probes.outcome).
const (
	ProbeFree  = "free"
	ProbeTaken = "taken"
	ProbeError = "error"
)

const (
	// probeBudget bounds one listing of the bucket.
	probeBudget = 30 * time.Second
	// probeLease holds a claimed probe: the listing and a margin to record the answer.
	probeLease = 2 * probeBudget
	// probeKeep is how long a probe row outlives its expiry before agentd deletes it.
	probeKeep = time.Hour
	// probeWorkers bounds the listings in flight at once. Each probe is an independent listing, so a
	// rotation of hundreds of agents is answered in a few listing latencies, not one per agent.
	probeWorkers = 16
)

// probeTick answers the due probes of every org and deletes the old rows.
func (p *Pool) probeTick(ctx context.Context) {
	orgs, err := p.q.ListOrgIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("repository probes: listing orgs failed", "err", err)
		}
		return
	}
	sem := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for _, org := range orgs {
		for ctx.Err() == nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			pr, err := p.q.ClaimHomeRepositoryProbe(ctx, store.ClaimHomeRepositoryProbeParams{OrgID: org, LeaseSeconds: int32(probeLease.Seconds())})
			if err != nil {
				<-sem
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					p.log.Warn("repository probes: claiming a probe failed", "err", err)
				}
				break
			}
			wg.Add(1)
			go safego.Do(p.log, "repository-probe-answer", func() {
				defer wg.Done()
				defer func() { <-sem }()
				p.answerProbe(ctx, pr)
			})
		}
		if _, err := p.q.DeleteOldHomeRepositoryProbes(ctx, store.DeleteOldHomeRepositoryProbesParams{OrgID: org, GraceSeconds: int32(probeKeep.Seconds())}); err != nil && ctx.Err() == nil {
			p.log.Warn("repository probes: deleting old probes failed", "err", err)
		}
	}
	wg.Wait()
}

// answerProbe checks the probe's prefix and records the answer.
func (p *Pool) answerProbe(ctx context.Context, pr store.HomeRepositoryProbe) {
	lctx, cancel := context.WithTimeout(ctx, probeBudget)
	held, err := p.cfg.Checkpoints.Store.HeldEpochs(lctx, pr.OrgID.String(), pr.AgentID.String())
	cancel()
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and another replica answers
	}
	outcome, msg := ProbeFree, ""
	switch {
	case err != nil:
		// Logged in full; the row gets a message without the endpoint's text, which can name the bucket.
		p.log.Warn("repository probes: listing the bucket failed", "agent", pr.AgentID.String(), "repository_epoch", pr.RepositoryEpoch, "err", err)
		outcome, msg = ProbeError, "koinon-agentd could not list the checkpoint bucket (see its log)"
	case slices.Contains(held, pr.RepositoryEpoch):
		outcome = ProbeTaken
		msg = fmt.Sprintf("the prefix of repository epoch %d already holds objects", pr.RepositoryEpoch)
	}
	if _, err := p.q.AnswerHomeRepositoryProbe(ctx, store.AnswerHomeRepositoryProbeParams{
		OrgID: pr.OrgID, ID: pr.ID, Outcome: pgtype.Text{String: outcome, Valid: true}, Message: msg,
	}); err != nil && ctx.Err() == nil {
		p.log.Warn("repository probes: recording the answer failed", "agent", pr.AgentID.String(), "err", err)
	}
}
