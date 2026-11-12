// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// NewRepositoriesAction is the audit action of keys new-repositories, one event per agent, in the
// agent's org.
const NewRepositoriesAction = "home.repository_replaced"

// newRepositoriesActor names the command in the audit log (actor kind cli).
var newRepositoriesActor = audit.CLI("keys new-repositories")

// newRepositoriesWord is what the person types to confirm keys new-repositories.
const newRepositoriesWord = "replace"

// NewRepositoriesResult is what NewRepositories changed.
type NewRepositoriesResult struct {
	// Agents is how many agents got a new home repository, in Orgs orgs.
	Agents, Orgs int
}

// homeTarget is an agent with a home repository.
type homeTarget struct{ org, agent pgtype.UUID }

// NewRepositories replaces the home repositories of the given agents, or of every agent with all,
// after a compromise (ADR 0085 decision 14): a KEK, a repository password or the checkpoint bucket
// may have leaked. Re-sealing a password (keys reencrypt) or changing it in kopia does not revoke
// it, so each agent gets a new repository epoch: a new kopia repository with a new master key and a
// random password sealed with the active KEK, which nobody has seen. koinon-agentd then seeds it
// with the agent's newest restorable checkpoint, and the sweeper deletes the old repository as soon
// as the new one holds that home (its deadline is now): history older than the seed is dropped.
//
// Before it writes anything it asks koinon-agentd, which holds the bucket's credentials where this
// command holds the KEKs (ADR 0032), whether the prefix of each new epoch already holds a
// repository (#285); a taken prefix, an error or no answer in time refuses the command (the
// recorded floor, home_repositories.next_epoch_at_least, stays the second guard, #262).
//
// Each agent changes in one transaction with its audit event ([NewRepositoriesAction], actor cli).
// The command does not wait for the seeds (koinon_agentd_home_repository_seeds). It does not rotate
// the bucket's S3 credentials: the operator does that at the S3 provider (the secret keys page of the docs site, project.SecretKeysDocURL).
// Like keys forget, it asks on a terminal and needs --yes without one. Agents without a home
// repository are skipped: their first one is made under the active KEK.
func (t *Tool) NewRepositories(ctx context.Context, all bool, agents []string) error {
	if all == (len(agents) > 0) {
		return errors.New("name the agents with --agent, or pass --all for every agent")
	}
	if t.Pool == nil {
		return errors.New("keys new-repositories needs --database-url")
	}
	targets, err := t.homeTargets(ctx, all, agents)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(t.Out, "No agent has a home repository; nothing to replace.")
		return nil
	}
	if err := t.confirmNewRepositories(ctx, len(targets)); err != nil {
		return err
	}
	// Loaded after the question, which can stay open for minutes: the new passwords are sealed with
	// the KEK that is active now.
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	ring, err := t.ring(ks)
	if err != nil {
		return err
	}
	// koinon-agentd, which holds the bucket's credentials, checks that no prefix a new epoch would
	// start holds a repository (#285). Nothing is written before every answer is free.
	probed, err := t.probeEpochs(ctx, targets)
	if err != nil {
		return err
	}
	var res NewRepositoriesResult
	orgs := map[pgtype.UUID]bool{}
	var errs []error
	for _, h := range targets {
		ok, err := t.newRepository(ctx, ring, h, probed)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent %s (org %s): %w", h.agent.String(), h.org.String(), err))
			continue
		}
		if ok {
			res.Agents++
			orgs[h.org] = true
		}
	}
	res.Orgs = len(orgs)
	t.reportNewRepositories(res)
	return errors.Join(errs...)
}

// homeTargets lists the agents with a home repository: all of them, or those named, each of which
// must have one.
func (t *Tool) homeTargets(ctx context.Context, all bool, agents []string) ([]homeTarget, error) {
	var ids []pgtype.UUID
	for _, a := range agents {
		var id pgtype.UUID
		if err := id.Scan(strings.TrimSpace(a)); err != nil {
			return nil, fmt.Errorf("%q is not an agent ID", a)
		}
		ids = append(ids, id)
	}
	rows, err := t.Pool.Query(ctx, `SELECT org_id, agent_id FROM home_repositories
		WHERE $1 OR agent_id = ANY($2::uuid[]) ORDER BY org_id, agent_id`, all, ids)
	if err != nil {
		return nil, err
	}
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (homeTarget, error) {
		var h homeTarget
		err := r.Scan(&h.org, &h.agent)
		return h, err
	})
	if err != nil || all {
		return targets, err
	}
	found := map[pgtype.UUID]bool{}
	for _, h := range targets {
		found[h.agent] = true
	}
	var missing []string
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id.String())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no home repository for agent(s) %s: nothing was changed", strings.Join(missing, ", "))
	}
	return targets, nil
}

// errNewRepositoriesUnconfirmed refuses keys new-repositories where nobody can be asked and --yes
// was not given.
var errNewRepositoriesUnconfirmed = errors.New("keys new-repositories drops the agents' older checkpoints: run it on a terminal (kubectl exec -it) or pass --yes")

// confirmNewRepositories asks the person to type newRepositoriesWord: the older checkpoints are
// deleted, which cannot be undone. As with keys forget, only --yes skips it.
func (t *Tool) confirmNewRepositories(ctx context.Context, n int) error {
	if t.Yes {
		return nil
	}
	if t.Prompt == nil {
		return errNewRepositoriesUnconfirmed
	}
	got, err := t.Prompt.Input(ctx,
		fmt.Sprintf("Replace the home repositories of %d agent(s)? Type %q to continue", n, newRepositoriesWord),
		"Each agent keeps only its newest checkpoint, copied into a new repository; the older checkpoints are deleted. Rotate the checkpoint bucket's S3 key as well ("+project.SecretKeysDocURL+").",
		func(s string) error {
			if strings.TrimSpace(s) != newRepositoriesWord {
				return fmt.Errorf("type %s to continue", newRepositoriesWord)
			}
			return nil
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != newRepositoriesWord {
		return cli.ErrCancelled
	}
	return nil
}

// newRepository starts the agent's next repository epoch, asks for its seed and audits both, in
// one transaction. False: the agent has no home repository any more.
func (t *Tool) newRepository(ctx context.Context, ring *secretstore.Keyring, h homeTarget, probed map[probeKey]int32) (bool, error) {
	ok := false
	_, err := audit.InTx(ctx, t.Pool, func(q *store.Queries, b audit.Batch) error {
		pw := checkpoint.Passwords{Q: q, Keys: ring}
		// A deadline of now: the sweeper's guard keeps the old repository until the seed is there.
		r, started, err := pw.NewEpochIn(ctx, q, h.org, h.agent, checkpoint.ReasonKeyCompromised, 0)
		if err != nil || !started {
			return err
		}
		if err := checkEpoch(probed, h.org, h.agent, r.Epoch); err != nil {
			return err
		}
		// An older retired repository (kept by a retention, as keys forget leaves one) may be under
		// an exposed password too: it is due now as well, so only the seed's copy survives. The
		// sweeper's guard keeps each until the new repository holds the newest home.
		if _, err := q.ExpireRetiredHomeRepository(ctx, store.ExpireRetiredHomeRepositoryParams{OrgID: h.org, AgentID: h.agent}); err != nil {
			return err
		}
		if _, err := q.ExpireRetiredHomeRepositoryEpochs(ctx, store.ExpireRetiredHomeRepositoryEpochsParams{OrgID: h.org, AgentID: h.agent}); err != nil {
			return err
		}
		prev := r.Replaced // not always r.Epoch - 1: a taken prefix is skipped (#262)
		n, err := q.RequestHomeRepositorySeed(ctx, store.RequestHomeRepositorySeedParams{
			OrgID: h.org, AgentID: h.agent, RepositoryEpoch: r.Epoch, SeedFromEpoch: pgtype.Int4{Int32: prev, Valid: true},
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("the new repository epoch %d was not marked for its seed", r.Epoch)
		}
		ok = true
		b.Org(audit.Event{
			Org: h.org, Actor: newRepositoriesActor, Action: NewRepositoriesAction, TargetKind: "agent", TargetID: h.agent.String(),
			Before: map[string]any{"repositoryEpoch": prev},
			After:  map[string]any{"repositoryEpoch": r.Epoch, "reason": checkpoint.ReasonKeyCompromised, "seedFromEpoch": prev},
		})
		return nil
	})
	return ok, err
}

// reportNewRepositories prints what NewRepositories changed and what the operator does next.
func (t *Tool) reportNewRepositories(res NewRepositoriesResult) {
	_, _ = fmt.Fprintf(t.Out, "Started a new home repository for %d agent(s) in %d org(s).\n", res.Agents, res.Orgs)
	if res.Agents == 0 {
		return
	}
	_, _ = fmt.Fprintln(t.Out, "koinon-agentd copies each agent's newest checkpoint into its new repository, then deletes the old one;")
	_, _ = fmt.Fprintln(t.Out, "koinon_agentd_home_repository_seeds{state=\"pending\"} falls to 0 when it is done, and {state=\"failed\"} counts the seeds it gave up on.")
	_, _ = fmt.Fprintln(t.Out, "Rotate the checkpoint bucket's S3 key at your S3 provider and restart koinon-agentd, if you have not yet ("+project.SecretKeysDocURL+").")
}
