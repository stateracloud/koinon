// SPDX-License-Identifier: Apache-2.0

package agentd

// checkpoints resync (ADR 0085 decision 17; #89).
//
// After a database restore, home_checkpoints and the bucket disagree: the bucket is not restored
// with the database, so it holds snapshots written after the dump, and kopia's retention has since
// deleted snapshots the dump still names. `koinon-api checkpoints resync` records a request per
// agent (home_checkpoint_resyncs); koinon-agentd, the only holder of the bucket's credentials,
// rebuilds the agent's checkpoints from the bucket, which is the truth for what exists:
//
//   - Every repository epoch of the agent that still opens is listed: the current one and retired
//     ones not yet deleted. A deleted epoch has no blobs and a lost one no password; both are
//     skipped (their checkpoints are not restorable already).
//   - A snapshot no checkpoint of its epoch names becomes a complete checkpoint, with what its tags
//     say (checkpoint.Meta): ownership epoch, generation, reason and sandbox. The checkpoint its
//     tag names gives its ID when the database has no such row (written after the dump), or its
//     reason, sandbox and, when complete in an earlier repository epoch (a seed's copy), its times.
//     It is left out while a backup is still recording it (requested, or running within its lease),
//     when the attempt that wrote it was recorded as failed, when that checkpoint is complete in the
//     same repository epoch, when a later snapshot of the epoch carries the same checkpoint tag, and
//     when its tags are unreadable. A binary before the Ready hook saved a snapshot before it checked
//     tar, and its retry kept the checkpoint ID: these rules keep such a partial home out. One case
//     cannot be told apart: such a checkpoint written after the dump whose last attempt failed after
//     its save, the database having no row to say so.
//   - The agent's ownership epoch is raised to the newest epoch of an added checkpoint. After a
//     restore it is the dump's, and the restore reads only checkpoints of epochs a run has seen
//     (ADR 0032): the next claim's run then sees the added ones and restores the newest home
//     instead of a pre-dump one.
//   - A complete checkpoint whose snapshot is not in the listing is marked failed, not deleted:
//     restore and the sweeper's guard skip it, and its history stays. Its rows are read before the
//     listing, and a checkpoint is complete only after its snapshot is saved, so a backup that
//     finishes during the resync is never marked.
//   - A repository the bucket does not hold is an empty one when the sweeper may have deleted it
//     (retired and past its deadline: the dump can predate the sweep). Otherwise, with checkpoints
//     that name it, the try fails and nothing changes: koinon-agentd may be using another bucket.
//   - A repository in a folder after the agent's newest epoch (one listing of the agent's epoch
//     folders), or at the prefix of an epoch with no checkpoints whose password does not open it,
//     was started after the dump (keys forget or keys new-repositories). It is recorded
//     (prefixtaken.go): the agent's next epoch starts past every held folder, and the sweeper
//     never deletes it. Its homes cannot be read without the lost password. The summary counts
//     the taken prefixes.
//
// The bucket is listed outside any transaction; the changes are then made in one transaction per
// agent under the agent's home_repositories row lock (LockHomeRepository, which starting a
// repository epoch takes too) together with the outcome and its audit event, and only for the
// claim that made the try (request_id and attempts): a try that outlived its lease, or whose
// request was replaced, changes nothing. A resync waits while the agent has a seed to do
// (ClaimHomeCheckpointResync): the seed records its copy itself. Running it twice changes nothing
// the second time.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The audit actions of a resync, actor system, in the agent's org.
const (
	ResyncedAction     = "home.checkpoints_resynced"
	ResyncFailedAction = "home.checkpoints_resync_failed"
)

// errResyncClaimLost: the try outlived its lease and another claimed the resync, or a new request
// replaced it; that one records the outcome.
var errResyncClaimLost = errors.New("the resync was claimed again or requested anew")

// resyncTick works through the due resyncs of every org, one agent at a time (a resync only lists
// the bucket, so it needs no backup slot), and sets the resync gauge.
func (p *Pool) resyncTick(ctx context.Context) {
	orgs, err := p.q.ListOrgIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("checkpoint resync: listing orgs failed", "err", err)
		}
		return
	}
	c := p.cfg.Checkpoints
	// The try's budget and a Timeout more to record its outcome.
	lease := int32((2 * c.Timeout).Seconds())
	for _, org := range orgs {
		for ctx.Err() == nil {
			r, err := p.q.ClaimHomeCheckpointResync(ctx, store.ClaimHomeCheckpointResyncParams{OrgID: org, LeaseSeconds: lease})
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					p.log.Warn("checkpoint resync: claiming a resync failed", "err", err)
				}
				break
			}
			p.resyncAgent(ctx, r)
		}
	}
	var pending, failed int64
	for _, org := range orgs {
		n, err := p.q.CountHomeCheckpointResyncs(ctx, org)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("checkpoint resync: counting failed", "err", err)
			}
			return
		}
		pending += n.Pending
		failed += n.Failed
	}
	p.cfg.Metrics.HomeCheckpointResyncs.WithLabelValues("pending").Set(float64(pending))
	p.cfg.Metrics.HomeCheckpointResyncs.WithLabelValues("failed").Set(float64(failed))
}

// resyncCounts is what one resync found and changed.
type resyncCounts struct {
	snapshots, added, missing, skipped int32
	// taken counts the agent's prefixes known to hold a repository the database has no password
	// for (#262): rows marked taken, and repositories after the newest epoch.
	taken  int32
	epochs []int32
	// ownership is the newest ownership epoch of an added checkpoint; raised to the agent's.
	ownership int64
	raised    bool // the agent's ownership epoch was raised to ownership
}

// repositoryListing is one repository epoch's complete checkpoints, read before its snapshots were
// listed, and the snapshots.
type repositoryListing struct {
	epoch  int32
	before []store.ListCompleteHomeCheckpointsOfRepositoryRow
	snaps  []checkpoint.Snapshot
}

// resyncAgent does one claimed resync and records the outcome.
func (p *Pool) resyncAgent(ctx context.Context, r store.HomeCheckpointResync) {
	c := p.cfg.Checkpoints
	log := p.log.With("agent", r.AgentID.String(), "attempt", r.Attempts)
	tctx, cancel := context.WithTimeout(ctx, c.Timeout)
	listings, taken, err := p.listRepositories(tctx, r)
	cancel()
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and another replica takes the resync over
	}
	if err == nil {
		var n resyncCounts
		n, err = p.recordResync(ctx, r, listings, taken)
		if err == nil {
			p.cfg.Metrics.HomeCheckpointResyncChanges.WithLabelValues("added").Add(float64(n.added))
			p.cfg.Metrics.HomeCheckpointResyncChanges.WithLabelValues("missing").Add(float64(n.missing))
			log.Info("home checkpoints resynced from the bucket", "snapshots", n.snapshots, "added", n.added, "missing", n.missing, "skipped", n.skipped, "taken", n.taken)
			return
		}
		if errors.Is(err, errResyncClaimLost) {
			log.Warn("checkpoint resync: the try outlived its claim or was requested anew; it changes nothing")
			return
		}
	}
	msg := truncateText(err.Error(), 400)
	if int(r.Attempts) >= c.Attempts {
		log.Error("checkpoint resync failed; the agent's checkpoints are as they were (see koinon_agentd_home_checkpoint_resyncs)", "err", msg)
		if _, err := audit.InPgTx(ctx, p.db, func(tx pgx.Tx, b audit.Batch) error {
			q := p.q.WithTx(tx)
			if n, err := q.FailHomeCheckpointResync(ctx, store.FailHomeCheckpointResyncParams{
				OrgID: r.OrgID, AgentID: r.AgentID, RequestID: r.RequestID, Attempts: r.Attempts, Error: pgtype.Text{String: msg, Valid: true},
			}); err != nil {
				return err
			} else if n != 1 {
				return errResyncClaimLost
			}
			b.Org(audit.Event{
				Org: r.OrgID, Actor: audit.System("koinon-agentd:resync"), Action: ResyncFailedAction, TargetKind: "agent", TargetID: r.AgentID.String(),
				After: map[string]any{"attempts": r.Attempts, "error": msg},
			})
			return nil
		}); err != nil {
			log.Warn("recording a failed checkpoint resync failed", "err", err)
		}
		return
	}
	delay := c.Retry << (r.Attempts - 1)
	log.Warn("checkpoint resync attempt failed; trying again", "err", msg, "in", delay)
	rel := store.ReleaseHomeCheckpointResyncParams{
		OrgID: r.OrgID, AgentID: r.AgentID, RequestID: r.RequestID, Attempts: r.Attempts,
		Error: pgtype.Text{String: msg, Valid: true}, DelaySeconds: int32(delay.Seconds()),
	}
	if _, err := p.q.ReleaseHomeCheckpointResync(ctx, rel); err != nil {
		log.Warn("releasing a checkpoint resync failed", "err", err)
	}
}

// listRepositories lists the snapshots of each of the agent's repositories that still opens,
// each after reading the checkpoints the database has for it, and counts the agent's taken prefixes.
func (p *Pool) listRepositories(ctx context.Context, r store.HomeCheckpointResync) ([]repositoryListing, int32, error) {
	c := p.cfg.Checkpoints
	epochs, err := p.q.ListHomeRepositoryEpochsOfAgent(ctx, store.ListHomeRepositoryEpochsOfAgentParams{OrgID: r.OrgID, AgentID: r.AgentID})
	if err != nil {
		return nil, 0, err
	}
	var out []repositoryListing
	var taken int32
	newest := int32(-1)
	for _, e := range epochs {
		newest = max(newest, e.RepositoryEpoch)
		if e.Taken {
			taken++
		}
		if e.Deleted || e.Lost || e.Taken {
			continue // nothing restores from it; its checkpoints are already skipped
		}
		before, err := p.q.ListCompleteHomeCheckpointsOfRepository(ctx, store.ListCompleteHomeCheckpointsOfRepositoryParams{
			OrgID: r.OrgID, AgentID: r.AgentID, RepositoryEpoch: e.RepositoryEpoch,
		})
		if err != nil {
			return nil, 0, err
		}
		repo, err := c.Repositories.At(ctx, r.OrgID, r.AgentID, e.RepositoryEpoch)
		if errors.Is(err, checkpoint.ErrRepositoryLost) || errors.Is(err, checkpoint.ErrNoRepository) {
			continue // lost since the epochs were read
		} else if err != nil {
			return nil, 0, fmt.Errorf("repository epoch %d: %w", e.RepositoryEpoch, err)
		}
		snaps, err := c.Store.Snapshots(ctx, repo.Prefix, repo.Password)
		switch {
		case checkpoint.TakenPrefix(err, repo.Prefix) && len(before) == 0:
			// Another repository, started after the dump at a prefix this epoch reused (#262): none of
			// the row's is there. Recorded, so the sweeper keeps those blobs and the next epoch skips it.
			if _, _, err := p.recordTakenPrefix(ctx, r.OrgID, r.AgentID, e.RepositoryEpoch, true, nil); err != nil {
				return nil, 0, fmt.Errorf("repository epoch %d: %w", e.RepositoryEpoch, err)
			}
			taken++
			continue
		case errors.Is(err, checkpoint.ErrNoRepositoryInBucket) && (e.Due || len(before) == 0):
			// Swept (the dump predates the sweep), or never written to: it holds nothing.
			snaps = nil
		case errors.Is(err, checkpoint.ErrNoRepositoryInBucket):
			return nil, 0, fmt.Errorf("repository epoch %d is not in the bucket, but %d complete checkpoints name it and it was not due for deletion; nothing was changed: is koinon-agentd using the bucket this database belongs to?", e.RepositoryEpoch, len(before))
		case err != nil:
			return nil, 0, fmt.Errorf("listing the snapshots of repository epoch %d: %w", e.RepositoryEpoch, err)
		}
		out = append(out, repositoryListing{epoch: e.RepositoryEpoch, before: before, snaps: snaps})
	}
	if newest >= 0 {
		// A repository after the newest epoch the database has was started after the dump (keys
		// forget or keys new-repositories): the next epoch must start past every one (#262). One
		// listing of the agent's prefix finds them, gaps included.
		held, err := c.Store.HeldEpochs(ctx, r.OrgID.String(), r.AgentID.String())
		if err != nil {
			return nil, 0, err
		}
		var after []int32
		for _, n := range held {
			if n > newest {
				after = append(after, n)
			}
		}
		if len(after) > 0 {
			// Re-checked under the lock against every folder, not the first only: a keys command
			// that started after[0] meanwhile leaves after[1:] taken.
			_, recorded, err := p.recordTakenPrefix(ctx, r.OrgID, r.AgentID, after[0], false, after)
			if err != nil {
				return nil, 0, fmt.Errorf("repository epoch %d: %w", after[0], err)
			}
			taken += recorded
		}
	}
	return out, taken, nil
}

// recordResync makes the listings' changes, completes the resync and audits it, in one
// transaction under the agent's repository row lock.
func (p *Pool) recordResync(ctx context.Context, r store.HomeCheckpointResync, listings []repositoryListing, taken int32) (resyncCounts, error) {
	var n resyncCounts
	_, err := audit.InPgTx(ctx, p.db, func(tx pgx.Tx, b audit.Batch) error {
		n = resyncCounts{taken: taken}
		q := p.q.WithTx(tx)
		// The agent row first, as a claim and a cascading delete take it, then the repository row.
		if _, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: r.OrgID, ID: r.AgentID}); err != nil {
			return fmt.Errorf("locking the agent: %w", err)
		}
		if _, err := q.LockHomeRepository(ctx, store.LockHomeRepositoryParams{OrgID: r.OrgID, AgentID: r.AgentID}); err != nil {
			return fmt.Errorf("locking the agent's repository: %w", err)
		}
		for _, l := range listings {
			n.epochs = append(n.epochs, l.epoch)
			known := map[string]bool{}
			for _, b := range l.before {
				known[b.SnapshotID] = true
			}
			last := lastPerCheckpoint(l.snaps)
			inBucket := map[string]bool{}
			for _, s := range l.snaps {
				inBucket[s.ID] = true
				n.snapshots++
				if known[s.ID] {
					continue
				}
				if id, ok := last[s.Meta.CheckpointID]; ok && id != s.ID {
					// An earlier attempt of the same checkpoint: a binary before the Ready hook saved
					// the snapshot before it checked tar, and its retry kept the checkpoint ID.
					n.skipped++
					continue
				}
				added, err := p.adoptSnapshot(ctx, q, r, l.epoch, s)
				if err != nil {
					return err
				}
				switch added {
				case adopted:
					n.added++
					n.ownership = max(n.ownership, s.Meta.Epoch)
				case leftOut:
					n.skipped++
				}
			}
			for _, b := range l.before {
				if inBucket[b.SnapshotID] {
					continue
				}
				m, err := q.MarkHomeCheckpointSnapshotMissing(ctx, store.MarkHomeCheckpointSnapshotMissingParams{
					OrgID: r.OrgID, ID: b.ID, SnapshotID: b.SnapshotID,
					Error: pgtype.Text{String: fmt.Sprintf("checkpoints resync: snapshot %s is no longer in the bucket (repository epoch %d)", b.SnapshotID, l.epoch), Valid: true},
				})
				if err != nil {
					return err
				}
				n.missing += int32(m) //nolint:gosec // 0 or 1
			}
		}
		if m, err := q.CompleteHomeCheckpointResync(ctx, store.CompleteHomeCheckpointResyncParams{
			OrgID: r.OrgID, AgentID: r.AgentID, RequestID: r.RequestID, Attempts: r.Attempts,
			Snapshots: pgtype.Int4{Int32: n.snapshots, Valid: true}, Added: pgtype.Int4{Int32: n.added, Valid: true},
			Missing: pgtype.Int4{Int32: n.missing, Valid: true}, Skipped: pgtype.Int4{Int32: n.skipped, Valid: true},
			Taken: pgtype.Int4{Int32: n.taken, Valid: true},
		}); err != nil {
			return err
		} else if m != 1 {
			return errResyncClaimLost // rolls the changes back: the claim that holds the resync makes them
		}
		raised, err := q.RaiseAgentEpochForResync(ctx, store.RaiseAgentEpochForResyncParams{
			OrgID: r.OrgID, AgentID: r.AgentID, Epoch: n.ownership,
		})
		if err != nil {
			return err
		}
		n.raised = raised == 1
		b.Org(audit.Event{
			Org: r.OrgID, Actor: audit.System("koinon-agentd:resync"), Action: ResyncedAction, TargetKind: "agent", TargetID: r.AgentID.String(),
			After: map[string]any{
				"repositoryEpochs": slices.Clone(n.epochs), "snapshots": n.snapshots, "added": n.added,
				"missing": n.missing, "skipped": n.skipped, "takenPrefixes": n.taken, "ownershipEpochRaisedTo": raisedTo(n),
			},
		})
		return nil
	})
	return n, err
}

// raisedTo is the ownership epoch the resync raised the agent's to, or nil.
func raisedTo(n resyncCounts) any {
	if !n.raised {
		return nil
	}
	return n.ownership
}

// lastPerCheckpoint maps each checkpoint ID that tags snapshots of one repository to the latest
// started of them (snaps are in start order). Tags that are not checkpoint IDs are not grouped.
func lastPerCheckpoint(snaps []checkpoint.Snapshot) map[string]string {
	last := map[string]string{}
	for _, s := range snaps {
		if s.MetaErr != nil {
			continue
		}
		if _, err := uuid.Parse(s.Meta.CheckpointID); err == nil {
			last[s.Meta.CheckpointID] = s.ID
		}
	}
	return last
}

// adoption is what adoptSnapshot did with a snapshot.
type adoption int

const (
	adopted adoption = iota // recorded as a complete checkpoint
	known                   // a checkpoint names it already (recorded since the listing)
	leftOut                 // a backup is still recording it, its attempt failed, or its tags are unreadable
)

// adoptSnapshot records a snapshot of repository epoch that no checkpoint of the epoch named when
// it was listed.
func (p *Pool) adoptSnapshot(ctx context.Context, q *store.Queries, r store.HomeCheckpointResync, epoch int32, s checkpoint.Snapshot) (adoption, error) {
	if s.MetaErr != nil {
		p.log.Warn("checkpoint resync: a snapshot's tags are unreadable; it is left out", "agent", r.AgentID.String(), "snapshot", s.ID, "err", s.MetaErr)
		return leftOut, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	cp := store.InsertResyncedHomeCheckpointParams{
		OrgID: r.OrgID, ID: pgtype.UUID{Bytes: id, Valid: true}, AgentID: r.AgentID,
		// A snapshot written before reasons were stamped, with no checkpoint to say: hourly, the
		// reason nothing waits on.
		Reason:    store.CheckpointReasonHourly,
		SandboxID: s.Meta.Sandbox, Generation: s.Meta.Generation, Epoch: s.Meta.Epoch, SnapshotID: s.ID,
		SizeBytes:   pgtype.Int8{Int64: s.Bytes, Valid: true},
		RequestedAt: pgtype.Timestamptz{Time: s.Start, Valid: true}, StartedAt: pgtype.Timestamptz{Time: s.Start, Valid: true},
		FinishedAt: pgtype.Timestamptz{Time: s.End, Valid: true}, RepositoryEpoch: epoch,
	}
	switch reason := store.CheckpointReason(s.Meta.Reason); reason {
	case store.CheckpointReasonHourly, store.CheckpointReasonStop, store.CheckpointReasonRecreate:
		cp.Reason = reason
	}
	var tag pgtype.UUID
	if tag.Scan(s.Meta.CheckpointID) == nil && tag.Valid {
		row, err := q.GetHomeCheckpointForResync(ctx, store.GetHomeCheckpointForResyncParams{
			OrgID: r.OrgID, ID: tag, LeaseSeconds: int32(p.cfg.Checkpoints.Lease.Seconds()),
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			cp.ID = tag // written after the dump: the checkpoint keeps its ID
		case err != nil:
			return 0, err
		case row.AgentID != r.AgentID:
			// Another agent's checkpoint ID: not this snapshot's, so the new ID stays.
		case row.InFlight:
			return leftOut, nil // its backup records it
		case row.Status == store.CheckpointStatusFailed:
			return leftOut, nil // the attempt that wrote it was given up
		case row.Status == store.CheckpointStatusComplete && row.RepositoryEpoch == epoch:
			// The checkpoint has its snapshot in this repository: this one is another attempt's
			// (a binary before the Ready hook saved it before it checked tar), maybe a partial home.
			return leftOut, nil
		default:
			cp.Reason, cp.SandboxID, cp.RequestedAt = row.Reason, row.SandboxID, row.RequestedAt
			if row.Status == store.CheckpointStatusComplete {
				// A seed's copy of that checkpoint's home into a later repository epoch: as old as it.
				cp.StartedAt, cp.FinishedAt = row.StartedAt, row.FinishedAt
			}
		}
	}
	if _, err := q.InsertResyncedHomeCheckpoint(ctx, cp); errors.Is(err, pgx.ErrNoRows) {
		return known, nil
	} else if err != nil {
		return 0, err
	}
	return adopted, nil
}
