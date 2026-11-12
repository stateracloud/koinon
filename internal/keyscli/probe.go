// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
)

// The outcomes of a storage probe, as koinon-agentd answers them (home_repository_probes.outcome).
// Kept here as text so that this package, which holds the KEKs, imports nothing of agentd's.
const (
	probeFree  = "free"
	probeTaken = "taken"
)

// DefaultProbeTimeout is how long keys new-repositories and keys forget wait for koinon-agentd to
// check the storage before they refuse the rotation.
const DefaultProbeTimeout = 2 * time.Minute

// probePerTarget is added to the default wait for every agent asked about, up to MaxProbeTimeout:
// a rotation of a large install needs more than a single check's time. An explicit ProbeTimeout is
// used as given.
const (
	probePerTarget  = 250 * time.Millisecond
	MaxProbeTimeout = 30 * time.Minute
)

// probePoll is how often the answer is looked for in the database.
const probePoll = time.Second

// probeKey is one agent and the epoch a rotation would start for it.
type probeKey struct{ org, agent pgtype.UUID }

// probeEpochs asks koinon-agentd, the only holder of the bucket's credentials (ADR 0032), whether
// the prefix of the next repository epoch of each agent in targets holds anything, and returns the
// epoch each was asked about. This process holds the KEKs and never the bucket's credentials: it
// writes probe requests (home_repository_probes) and waits for the answers. It fails closed
// (#285): an answer of taken or error, or none within the timeout, is an error, and the caller
// writes nothing. The rows are deleted when read (koinon-agentd deletes what a dead command left).
//
// The epoch is read here and again by the transaction that starts the epoch (checkEpoch compares
// them), so that a rotation that raced with another is refused instead of using a prefix nobody
// asked about.
func (t *Tool) probeEpochs(ctx context.Context, targets []homeTarget) (map[probeKey]int32, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	timeout := t.ProbeTimeout
	if timeout <= 0 {
		timeout = min(DefaultProbeTimeout+time.Duration(len(targets))*probePerTarget, MaxProbeTimeout)
	}
	poll := t.ProbePoll
	if poll <= 0 {
		poll = probePoll
	}
	q := store.New(t.Pool)
	type pending struct {
		h     homeTarget
		epoch int32
		id    pgtype.UUID
	}
	var reqs, asked []pending
	defer func() {
		// Not the caller's context: it may be the reason we return.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for _, r := range asked {
			_ = q.DeleteHomeRepositoryProbe(dctx, store.DeleteHomeRepositoryProbeParams{OrgID: r.h.org, ID: r.id})
		}
	}()
	epochs := map[probeKey]int32{}
	for _, h := range targets {
		next, err := q.LatestHomeRepositoryNextEpoch(ctx, store.LatestHomeRepositoryNextEpochParams{OrgID: h.org, AgentID: h.agent})
		if errors.Is(err, pgx.ErrNoRows) {
			// The agent's repository was deleted since the targets were read (the agent with it):
			// nothing to probe, and no epoch is started for it (NewEpochIn reports !started). Should
			// one start all the same, it is not in the map and checkEpoch refuses it.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("agent %s: reading the next repository epoch: %w", h.agent, err)
		}
		// The row expires a little after we stop waiting, so agentd never answers one nobody reads.
		row, err := q.RequestHomeRepositoryProbe(ctx, store.RequestHomeRepositoryProbeParams{
			OrgID: h.org, AgentID: h.agent, RepositoryEpoch: next, TimeoutSeconds: int32(timeout.Seconds()) + 1,
		})
		if err != nil {
			return nil, fmt.Errorf("agent %s: asking koinon-agentd to check the storage: %w", h.agent, err)
		}
		reqs = append(reqs, pending{h: h, epoch: next, id: row.ID})
		asked = reqs
		epochs[probeKey(h)] = next
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	var refused []error
	for len(reqs) > 0 {
		// The pending answers in one query per org and poll (the table is keyed (org_id, id)), not
		// one per agent.
		ids := map[pgtype.UUID][]pgtype.UUID{}
		for _, r := range reqs {
			ids[r.h.org] = append(ids[r.h.org], r.id)
		}
		byID := make(map[[2]pgtype.UUID]store.HomeRepositoryProbe, len(reqs))
		for org, of := range ids {
			rows, err := q.ListHomeRepositoryProbes(ctx, store.ListHomeRepositoryProbesParams{OrgID: org, Ids: of})
			if err != nil {
				return nil, fmt.Errorf("org %s: reading the storage checks: %w", org, err)
			}
			for _, row := range rows {
				byID[[2]pgtype.UUID{row.OrgID, row.ID}] = row
			}
		}
		var waiting []pending
		for _, r := range reqs {
			row, found := byID[[2]pgtype.UUID{r.h.org, r.id}]
			switch {
			case !found:
				refused = append(refused, fmt.Errorf("agent %s: the storage check was deleted before it was answered", r.h.agent))
			case !row.AnsweredAt.Valid:
				waiting = append(waiting, r)
			case row.Outcome.String == probeFree:
			case row.Outcome.String == probeTaken:
				refused = append(refused, fmt.Errorf("agent %s: the prefix of repository epoch %d already holds a repository the database does not record: "+
					"koinon-agentd recorded the next free epoch for the agent, run this command again to start the repository there (%s)",
					r.h.agent, r.epoch, project.SecretKeysDocURL))
			default:
				refused = append(refused, fmt.Errorf("agent %s: koinon-agentd could not check the storage prefix of repository epoch %d: %s",
					r.h.agent, r.epoch, row.Message.String))
			}
		}
		reqs = waiting
		if len(reqs) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			for _, r := range reqs {
				refused = append(refused, fmt.Errorf("agent %s: koinon-agentd did not check the storage within %s (is it running, and does it reach the checkpoint bucket?)", r.h.agent, timeout))
			}
			reqs = reqs[:0:0]
		case <-tick.C:
		}
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("refusing to start a new repository, nothing was changed: %w", errors.Join(refused...))
	}
	return epochs, nil
}

// checkEpoch is the guard after the transaction started the epoch: it must be the one the probe
// asked about. Another rotation, or koinon-agentd raising the floor, since the probe changes it;
// the transaction is then rolled back and the command run again.
func checkEpoch(probed map[probeKey]int32, org, agent pgtype.UUID, got int32) error {
	want, ok := probed[probeKey{org, agent}]
	if !ok {
		return fmt.Errorf("agent %s needs repository epoch %d, and its storage was not checked (its repository changed meanwhile): nothing was changed, run the command again", agent, got)
	}
	if want != got {
		return fmt.Errorf("the next repository epoch of agent %s changed while the storage was checked (checked %d, would start %d): nothing was changed, run the command again", agent, want, got)
	}
	return nil
}
