// SPDX-License-Identifier: Apache-2.0

package agentd

// Seeding a new home repository after a compromise (ADR 0085 decision 14; #86).
//
// `koinon-api keys new-repositories` starts a new repository epoch for each agent, with a password
// nobody has seen, retires the old one with a deadline of now and asks for a seed
// (home_repository_epochs.seed_from_epoch). koinon-agentd, the only holder of the bucket's
// credentials, copies the agent's newest restorable checkpoint into the new repository: it restores
// the snapshot from its own repository and streams the tar straight into a backup of the new one,
// without a sandbox. The copy is recorded as a checkpoint of the new epoch with the source's times,
// so it is no newer a home than the source; the sweeper's guard then lets the old repository go.
//
// A seed is claimed with a lease that covers a whole try (seedBudget, as a sandbox restore's claim
// covers its tries), and tried up to Checkpoints.Attempts times with a growing delay, each try
// falling back to older checkpoints (RestoreFallbacks) when the newest does not copy. A try's
// outcome is recorded only under its own claim (seed_attempts): a writer whose lease ran out, and
// whose seed another claimed since, changes nothing. Seeds hold at most Concurrency-1 backup slots. When every try fails the seed is given up (seed_failed_at, audited):
// the old repository is kept, because nothing newer holds the agent's home.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/safego"
)

// The audit actions of a seed, actor system, in the agent's org.
const (
	SeededAction     = "home.repository_seeded"
	SeedFailedAction = "home.repository_seed_failed"
)

// seedTick claims the seeds that are due, each in a backup slot (a seed stages a home on the
// scratch disk as a backup does) and one of the seeds' own slots, and sets the seed gauge.
func (p *Pool) seedTick(ctx context.Context, slots, seeds chan struct{}, wg *sync.WaitGroup) {
	orgs, err := p.q.ListOrgIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("repository seeds: listing orgs failed", "err", err)
		}
		return
	}
	var pending, failed int64
	counted := true
	for _, org := range orgs {
		n, err := p.q.CountHomeRepositorySeeds(ctx, org)
		if err != nil {
			counted = false
			if ctx.Err() == nil {
				p.log.Warn("repository seeds: counting failed", "err", err)
			}
		} else {
			pending += n.Pending
			failed += n.Failed
		}
		p.claimSeeds(ctx, org, slots, seeds, wg)
	}
	if counted {
		p.cfg.Metrics.HomeRepositorySeeds.WithLabelValues("pending").Set(float64(pending))
		p.cfg.Metrics.HomeRepositorySeeds.WithLabelValues("failed").Set(float64(failed))
	}
}

// seedBudget bounds one try of a seed: a copy of each checkpoint it may fall back to, each within
// Timeout, the waits for a restore slot included.
func (c *Checkpoints) seedBudget() time.Duration {
	return c.Timeout * time.Duration(1+c.RestoreFallbacks)
}

// claimSeeds starts the org's due seeds while a seed slot and a backup slot are free; the next tick
// takes the rest.
func (p *Pool) claimSeeds(ctx context.Context, org pgtype.UUID, slots, seeds chan struct{}, wg *sync.WaitGroup) {
	c := p.cfg.Checkpoints
	// The try's budget and a Timeout more to record its outcome, as a sandbox restore's claim.
	lease := int32((c.seedBudget() + c.Timeout).Seconds())
	for {
		select {
		case seeds <- struct{}{}:
		default:
			return // as many seeds as may run at once
		}
		select {
		case slots <- struct{}{}:
		default:
			<-seeds
			return // all writers busy
		}
		e, err := p.q.ClaimHomeRepositorySeed(ctx, store.ClaimHomeRepositorySeedParams{OrgID: org, LeaseSeconds: lease})
		if err != nil {
			<-slots
			<-seeds
			if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
				p.log.Warn("repository seeds: claiming a seed failed", "err", err)
			}
			return
		}
		wg.Go(func() {
			defer func() { <-slots; <-seeds }()
			// A panic is logged; the seed's lease runs out and it is tried again.
			safego.Do(p.log, "seed-repository", func() { p.seedRepository(ctx, e) })
		})
	}
}

var (
	// errSeedSuperseded: a later repository epoch started while the seed ran; that one is seeded.
	errSeedSuperseded = errors.New("a later repository epoch started")
	// errSeedClaimLost: the try outlived its lease and the seed was claimed again since; the
	// later claim records the outcome.
	errSeedClaimLost = errors.New("the seed was claimed again")
)

// seedRepository does one claimed seed and records the outcome.
func (p *Pool) seedRepository(ctx context.Context, e store.HomeRepositoryEpoch) {
	c := p.cfg.Checkpoints
	log := p.log.With("agent", e.AgentID.String(), "repository_epoch", e.RepositoryEpoch, "attempt", e.SeedAttempts)
	tctx, cancel := context.WithTimeout(ctx, c.seedBudget())
	src, seeded, err := p.copyNewestHome(tctx, e)
	cancel()
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and another replica takes the seed over
	}
	key := store.CompleteHomeRepositorySeedParams{OrgID: e.OrgID, AgentID: e.AgentID, RepositoryEpoch: e.RepositoryEpoch, SeedAttempts: e.SeedAttempts}
	if err == nil {
		_, err = audit.InPgTx(ctx, p.db, func(tx pgx.Tx, b audit.Batch) error {
			q := p.q.WithTx(tx)
			after := map[string]any{"repositoryEpoch": e.RepositoryEpoch}
			if seeded != nil {
				cp, err := q.InsertSeededHomeCheckpoint(ctx, store.InsertSeededHomeCheckpointParams{
					OrgID: e.OrgID, SourceID: src.ID, RepositoryEpoch: e.RepositoryEpoch,
					SnapshotID: pgtype.Text{String: seeded.SnapshotID, Valid: true}, SizeBytes: pgtype.Int8{Int64: seeded.Bytes, Valid: true},
				})
				if err != nil {
					return err
				}
				after["checkpoint"] = cp.ID.String()
				after["fromCheckpoint"] = src.ID.String()
				after["fromRepositoryEpoch"] = src.RepositoryEpoch
				after["takenAt"] = src.FinishedAt.Time.UTC()
			}
			if n, err := q.CompleteHomeRepositorySeed(ctx, key); err != nil {
				return err
			} else if n != 1 {
				return whySeedNotRecorded(ctx, q, e)
			}
			b.Org(audit.Event{
				Org: e.OrgID, Actor: audit.System("koinon-agentd:home-seed"), Action: SeededAction, TargetKind: "agent", TargetID: e.AgentID.String(), After: after,
			})
			return nil
		})
		if err == nil {
			if seeded != nil {
				log.Info("home repository seeded", "from_checkpoint", src.ID.String(), "from_repository_epoch", src.RepositoryEpoch, "bytes", seeded.Bytes)
			} else {
				log.Info("home repository seed: the agent's newest home is already in the new repository, or it has none")
			}
			return
		}
		if errors.Is(err, errSeedSuperseded) {
			log.Info("home repository seed dropped: a later repository epoch started, and that one is seeded")
			return
		}
		if errors.Is(err, errSeedClaimLost) {
			log.Warn("home repository seed: the try outlived its lease and the seed was claimed again; its copy is not recorded")
			return
		}
		log.Warn("recording a home repository seed failed", "err", err)
		// Not recorded: the copy is made again on the next try; its snapshot is left unreferenced.
	}
	msg := truncateText(err.Error(), 400)
	var perm *permanent
	if errors.As(err, &perm) || int(e.SeedAttempts) >= c.Attempts {
		log.Error("home repository seed failed; the old repository is kept (see koinon_agentd_home_repository_seeds)", "err", msg)
		if _, err := audit.InPgTx(ctx, p.db, func(tx pgx.Tx, b audit.Batch) error {
			q := p.q.WithTx(tx)
			if n, err := q.FailHomeRepositorySeed(ctx, store.FailHomeRepositorySeedParams{
				OrgID: e.OrgID, AgentID: e.AgentID, RepositoryEpoch: e.RepositoryEpoch, SeedAttempts: e.SeedAttempts,
				Error: pgtype.Text{String: msg, Valid: true},
			}); err != nil {
				return err
			} else if n != 1 {
				return whySeedNotRecorded(ctx, q, e)
			}
			b.Org(audit.Event{
				Org: e.OrgID, Actor: audit.System("koinon-agentd:home-seed"), Action: SeedFailedAction, TargetKind: "agent", TargetID: e.AgentID.String(),
				After: map[string]any{"repositoryEpoch": e.RepositoryEpoch, "attempts": e.SeedAttempts, "error": msg},
			})
			return nil
		}); errors.Is(err, errSeedSuperseded) || errors.Is(err, errSeedClaimLost) {
			log.Warn("home repository seed: not recorded as given up", "why", err)
		} else if err != nil {
			log.Warn("recording a failed home repository seed failed", "err", err)
		}
		return
	}
	delay := c.Retry << (e.SeedAttempts - 1)
	log.Warn("home repository seed attempt failed; trying again", "err", msg, "in", delay)
	if n, err := p.q.ReleaseHomeRepositorySeed(ctx, store.ReleaseHomeRepositorySeedParams{
		OrgID: e.OrgID, AgentID: e.AgentID, RepositoryEpoch: e.RepositoryEpoch, SeedAttempts: e.SeedAttempts,
		Error: pgtype.Text{String: msg, Valid: true}, DelaySeconds: int32(delay.Seconds()),
	}); err != nil {
		log.Warn("releasing a home repository seed failed", "err", err)
	} else if n != 1 {
		log.Warn("home repository seed: not released for a retry", "why", whySeedNotRecorded(ctx, p.q, e))
	}
}

// whySeedNotRecorded says why a seed's outcome matched no row: a later epoch superseded it, or
// another claim holds it (or recorded it already).
func whySeedNotRecorded(ctx context.Context, q *store.Queries, e store.HomeRepositoryEpoch) error {
	r, err := q.GetHomeRepositoryEpoch(ctx, store.GetHomeRepositoryEpochParams{OrgID: e.OrgID, AgentID: e.AgentID, RepositoryEpoch: e.RepositoryEpoch})
	if err != nil {
		return fmt.Errorf("reading the seed: %w", err)
	}
	if r.RetiredAt.Valid {
		return errSeedSuperseded
	}
	return errSeedClaimLost
}

// copyNewestHome copies the agent's newest restorable checkpoint into the repository of epoch e
// and returns the source and the copy. A nil copy: there is nothing to copy, because the newest
// checkpoint is already in e (a backup after the epoch started) or the agent has none.
func (p *Pool) copyNewestHome(ctx context.Context, e store.HomeRepositoryEpoch) (store.HomeCheckpoint, *checkpoint.Result, error) {
	c := p.cfg.Checkpoints
	cps, err := p.q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{
		OrgID: e.OrgID, AgentID: e.AgentID, MaxRows: int32(1 + c.RestoreFallbacks), //nolint:gosec // small
	})
	if err != nil {
		return store.HomeCheckpoint{}, nil, err
	}
	if len(cps) == 0 || cps[0].RepositoryEpoch >= e.RepositoryEpoch {
		return store.HomeCheckpoint{}, nil, nil
	}
	dst, err := c.Repositories.At(ctx, e.OrgID, e.AgentID, e.RepositoryEpoch)
	if errors.Is(err, checkpoint.ErrNoRepository) || errors.Is(err, checkpoint.ErrRepositoryLost) {
		return store.HomeCheckpoint{}, nil, &permanent{fmt.Errorf("the new repository cannot be opened: %w", err)}
	} else if err != nil {
		return store.HomeCheckpoint{}, nil, err
	}
	var errs []error
	for _, cp := range cps {
		if cp.RepositoryEpoch >= e.RepositoryEpoch {
			continue // written to the new repository after a newer one failed to copy: not a source
		}
		src, err := c.Repositories.At(ctx, e.OrgID, e.AgentID, cp.RepositoryEpoch)
		if errors.Is(err, checkpoint.ErrNoRepository) || errors.Is(err, checkpoint.ErrRepositoryLost) {
			errs = append(errs, fmt.Errorf("checkpoint %s (repository epoch %d): %w", cp.ID, cp.RepositoryEpoch, err))
			continue // an older checkpoint, in another epoch, may still copy
		} else if err != nil {
			return store.HomeCheckpoint{}, nil, err
		}
		res, err := p.copyCheckpoint(ctx, src, dst, cp)
		if err == nil {
			return cp, &res, nil
		}
		if ctx.Err() != nil {
			return store.HomeCheckpoint{}, nil, ctx.Err()
		}
		if checkpoint.TakenPrefix(err, dst.Prefix) {
			// No source copies into it: the seed fails, and the old repository is kept.
			return store.HomeCheckpoint{}, nil, p.prefixTaken(ctx, e.OrgID, e.AgentID, dst, err)
		}
		p.log.Warn("home repository seed: copying a checkpoint failed", "agent", e.AgentID.String(), "checkpoint", cp.ID.String(), "err", err)
		errs = append(errs, fmt.Errorf("checkpoint %s: %w", cp.ID, err))
	}
	return store.HomeCheckpoint{}, nil, fmt.Errorf("no checkpoint copied: %w", errors.Join(errs...))
}

// copyCheckpoint streams one snapshot from src into a new snapshot in dst, with the source's
// metadata, and verifies it. Both stage on the scratch disk: the seed holds a restore slot as well
// as its backup slot.
func (p *Pool) copyCheckpoint(ctx context.Context, src, dst checkpoint.Repository, cp store.HomeCheckpoint) (checkpoint.Result, error) {
	c := p.cfg.Checkpoints
	select {
	case c.restoreSlots <- struct{}{}:
		defer func() { <-c.restoreSlots }()
	case <-ctx.Done():
		return checkpoint.Result{}, ctx.Err()
	}
	actx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	pr, pw := io.Pipe()
	restored := make(chan error, 1)
	go func() {
		err := c.Store.Restore(actx, src.Prefix, src.Password, cp.SnapshotID.String, pw)
		_ = pw.CloseWithError(err) // nil closes with io.EOF
		restored <- err
	}()
	var rerr error
	gotRestore := false
	res, err := c.Store.Backup(actx, dst.Prefix, dst.Password, pr, checkpoint.Meta{
		CheckpointID: cp.ID.String(), Generation: cp.Generation, Epoch: cp.Epoch,
		Reason: string(cp.Reason), Sandbox: cp.SandboxID,
		// The source must have been read whole before the copy's snapshot is written: a snapshot of
		// a stream that ended early is not the source's home, and checkpoints resync records what
		// the bucket holds.
		Ready: func() error {
			_, _ = io.Copy(io.Discard, pr) // the archive's padding, up to the restore's end
			rerr, gotRestore = <-restored, true
			if rerr != nil {
				return fmt.Errorf("reading the source snapshot: %w", rerr)
			}
			return nil
		},
	})
	// The backup stops reading at the archive's end or on an error: unblock the writer either way.
	_ = pr.CloseWithError(errors.New("the backup ended"))
	if !gotRestore {
		rerr = <-restored
	}
	if err != nil {
		return checkpoint.Result{}, err
	}
	if rerr != nil {
		// The backup took a stream that ended early; the snapshot is not the source's home.
		return checkpoint.Result{}, fmt.Errorf("reading the source snapshot: %w", rerr)
	}
	if err := c.Store.Verify(actx, dst.Prefix, dst.Password, res.SnapshotID); err != nil {
		return checkpoint.Result{}, fmt.Errorf("verifying the copy: %w", err)
	}
	return res, nil
}
