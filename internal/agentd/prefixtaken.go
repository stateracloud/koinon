// SPDX-License-Identifier: Apache-2.0

package agentd

// A repository prefix that holds another repository (#262).
//
// A repository's prefix follows from the agent and its epoch (checkpoint.EpochPrefix). After a
// database restore, the prefix of an epoch that koinon-api keys forget or keys new-repositories
// starts can already hold a kopia repository started after the dump, whose password only the lost
// database had. koinon-agentd, the only holder of the bucket's credentials, finds it: a backup or a
// seed into it gets checkpoint.ErrPrefixTaken (the writer never creates a repository over it), and
// checkpoints resync looks at the prefixes after the agent's newest epoch. It then records, under
// the agent's repository row lock:
//
//   - the lowest free epoch above the taken prefixes (home_repositories.next_epoch_at_least): the
//     next keys forget or keys new-repositories starts the agent's repository there (koinon-api
//     holds the keys that seal a new password, agentd does not, ADR 0054);
//   - on the row whose prefix is taken (prefix_taken_at), that nothing of the row's is there, so
//     the sweeper never deletes those blobs once the row is retired.
//
// The backup or seed then fails with an error that names the prefix and the command to run. The
// homes in the repository found are not touched: they cannot be read without the lost password.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// PrefixTakenAction is audited (actor system, in the agent's org) when koinon-agentd finds that a
// prefix of the agent's repositories holds a repository the database has no password for.
const PrefixTakenAction = "home.repository_prefix_taken"

// errPrefixTaken is the error a backup or seed into a taken prefix fails with.
func errPrefixTaken(agent pgtype.UUID, prefix string, free int32) error {
	return fmt.Errorf("the repository prefix %s already holds a kopia repository this database has no password for "+
		"(one started after the dump the database was restored from?): run `koinon-api keys new-repositories --agent %s`, "+
		"which starts the agent's next repository at the free epoch %d (docs/ops/secret-keys.md)", prefix, agent, free)
}

// unopenedError is a repository epoch whose prefix does not open with the epoch's password although
// the database records complete checkpoints written there with it: not a repository started after
// a dump, but the wrong bucket or a damaged kopia.repository. Nothing is recorded for it, since
// another epoch would retire this one and let the sweeper delete the homes it holds.
type unopenedError struct {
	prefix      string
	checkpoints int
}

func (e *unopenedError) Error() string {
	return fmt.Sprintf("the repository at %s does not open with the password that wrote its %d complete checkpoint(s): "+
		"koinon-agentd uses the wrong bucket, or the repository's kopia.repository is damaged; do not start a new repository "+
		"for the agent, which would retire this one and its homes (docs/ops/secret-keys.md)", e.prefix, e.checkpoints)
}

// freeEpochAfter returns the first epoch after epoch and after every epoch of held, the agent's
// epoch folders that hold anything: one listing (HeldEpochs), so a gap the lost database's sweeper
// left (r<N+2>/ empty, r<N+3>/ held) does not end the search early.
func freeEpochAfter(epoch int32, held []int32) int32 {
	free := epoch + 1
	for _, n := range held {
		free = max(free, n+1)
	}
	return free
}

// recordTakenPrefix records that the prefix of the agent's repository epoch (one the database has,
// markRow, or the first of those after its newest, !markRow) holds a repository the database has no
// password for, and returns the free epoch the next repository starts at and, for !markRow, how many
// of the folders in held it recorded; free is 0 when nothing was recorded since a keys command
// started those epochs meanwhile (!markRow). It audits a change once. An epoch the database records
// complete checkpoints of is not taken: *unopenedError, and nothing is recorded.
//
// The bucket is listed before the transaction, never under the row lock, so a slow endpoint cannot
// hold the lock and a pool connection: markRow lists here; !markRow gets held, the caller's listing
// (the folders after the agent's newest epoch, ascending). The floor is computed from that earlier
// listing; under the lock the database is re-read (the latest epoch, the complete checkpoints), and
// a folder a keys command started since is left out.
func (p *Pool) recordTakenPrefix(ctx context.Context, org, agent pgtype.UUID, epoch int32, markRow bool, held []int32) (int32, int32, error) {
	if markRow {
		var err error
		if held, err = p.cfg.Checkpoints.Store.HeldEpochs(ctx, org.String(), agent.String()); err != nil {
			return 0, 0, fmt.Errorf("looking for a free repository prefix: %w", err)
		}
	}
	var free, recorded int32
	changed := false
	prefix := checkpoint.EpochPrefix(org.String(), agent.String(), epoch)
	err := pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		free, recorded, changed = 0, 0, false
		prefix = checkpoint.EpochPrefix(org.String(), agent.String(), epoch)
		q := p.q.WithTx(tx)
		if _, err := q.LockHomeRepository(ctx, store.LockHomeRepositoryParams{OrgID: org, AgentID: agent}); err != nil {
			return fmt.Errorf("locking the agent's repository: %w", err)
		}
		floor := freeEpochAfter(epoch, held)
		if markRow {
			complete, err := q.ListCompleteHomeCheckpointsOfRepository(ctx, store.ListCompleteHomeCheckpointsOfRepositoryParams{
				OrgID: org, AgentID: agent, RepositoryEpoch: epoch,
			})
			if err != nil {
				return err
			}
			if len(complete) > 0 {
				return &unopenedError{prefix: prefix, checkpoints: len(complete)}
			}
		} else {
			// The folders were listed before this lock: a keys command may have started the first
			// since, and then the repository found is the database's own. The folders after it can
			// still be taken, so the re-check is against the newest of them, not the first only.
			latest, err := q.LatestHomeRepositoryEpoch(ctx, store.LatestHomeRepositoryEpochParams{OrgID: org, AgentID: agent})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			var still []int32
			for _, n := range held {
				if n >= epoch && (err != nil || n > latest.RepositoryEpoch) {
					still = append(still, n)
				}
			}
			if len(still) == 0 {
				return nil
			}
			epoch, recorded = still[0], int32(len(still)) //nolint:gosec // a handful of folders
			prefix = checkpoint.EpochPrefix(org.String(), agent.String(), epoch)
		}
		var err error
		var marked int64
		if markRow && epoch == 0 {
			marked, err = q.MarkHomeRepositoryPrefixTaken(ctx, store.MarkHomeRepositoryPrefixTakenParams{OrgID: org, AgentID: agent})
		} else if markRow {
			marked, err = q.MarkHomeRepositoryEpochPrefixTaken(ctx, store.MarkHomeRepositoryEpochPrefixTakenParams{OrgID: org, AgentID: agent, RepositoryEpoch: epoch})
		}
		if err != nil {
			return err
		}
		raised, err := q.RaiseHomeRepositoryNextEpoch(ctx, store.RaiseHomeRepositoryNextEpochParams{OrgID: org, AgentID: agent, NextEpoch: floor})
		if err != nil {
			return err
		}
		free = floor
		if marked == 0 && raised == 0 {
			return nil // recorded already
		}
		changed = true
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: audit.System("koinon-agentd:prefix-taken"), Action: PrefixTakenAction, TargetKind: "agent", TargetID: agent.String(),
			After: map[string]any{
				"repositoryEpoch": epoch, "prefix": prefix, "knownToDatabase": markRow, "nextEpochAtLeast": floor,
			},
		})
	})
	if err != nil {
		return 0, 0, err
	}
	log := p.log.Info
	if changed {
		log = p.log.Error
	}
	if free > 0 {
		log("home repository: the prefix holds a repository this database has no password for; the agent's next repository starts at a free epoch",
			"org", org.String(), "agent", agent.String(), "repository_epoch", epoch, "next_epoch_at_least", free)
	}
	return free, recorded, nil
}

// prefixTaken turns err into the error a backup or seed into repo fails with when repo's prefix
// does not open with its password (after recording a taken prefix), and returns err unchanged
// otherwise.
func (p *Pool) prefixTaken(ctx context.Context, org, agent pgtype.UUID, repo checkpoint.Repository, err error) error {
	if !checkpoint.TakenPrefix(err, repo.Prefix) {
		return err
	}
	free, _, rerr := p.recordTakenPrefix(ctx, org, agent, repo.Epoch, true, nil)
	var unopened *unopenedError
	if errors.As(rerr, &unopened) {
		p.log.Error("home repository: the repository does not open with the password that wrote its checkpoints",
			"org", org.String(), "agent", agent.String(), "repository_epoch", repo.Epoch, "complete_checkpoints", unopened.checkpoints)
		return &permanent{unopened}
	}
	if rerr != nil {
		return errors.Join(err, fmt.Errorf("recording the taken prefix: %w", rerr))
	}
	return &permanent{errPrefixTaken(agent, repo.Prefix, free)}
}
