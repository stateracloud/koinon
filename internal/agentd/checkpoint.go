// SPDX-License-Identifier: Apache-2.0

package agentd

// Home checkpoints (plan item HC1; ADRs 0032, 0051; practice B3).
//
// koinon-agentd is the writer: it alone holds the storage credentials and the repositories'
// passwords (package checkpoint), and an S3 store's NetworkPolicy admits only agentd and
// the API. It reads the home out of the sandbox as a tar stream over exec, as saved work is read
// (savework.go), and streams it into the kopia writer.
//
// Requests come from Postgres (home_checkpoints). agentd makes the hourly ones for every ready
// sandbox; the reconciler makes the ones before an idle stop and before a recreation and waits on
// them (internal/reconciler/checkpoint.go). A request is claimed with a lease, tried up to
// CheckpointAttempts times with a growing delay, and then fails: a failed recreate request blocks
// the recreation.
//
// Restore runs in the first run that finds a recreated sandbox (restoreHome): it streams the
// newest checkpoint back in through OpenShell's flow-controlled upload channel before the harness
// starts, trying older ones when a restore fails. The backup reads the home as a tar stream over
// exec; the restore cannot write one over exec (see restoreOne).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/safego"
)

// HomeStore is the checkpoint writer as agentd uses it (checkpoint.Writer).
type HomeStore interface {
	Backup(ctx context.Context, prefix, password string, tarStream io.Reader, m checkpoint.Meta) (checkpoint.Result, error)
	Restore(ctx context.Context, prefix, password, snapshotID string, dst io.Writer) error
	Verify(ctx context.Context, prefix, password, snapshotID string) error
	// DeleteRepository deletes the blobs of a retired repository, never a later epoch's under it.
	DeleteRepository(ctx context.Context, prefix string) (int, error)
	// Snapshots lists a repository's complete snapshots, for checkpoints resync (resync.go).
	Snapshots(ctx context.Context, prefix, password string) ([]checkpoint.Snapshot, error)
	// HeldEpochs lists, in one listing, the agent's repository epochs after 0 whose folder in the
	// bucket holds anything, whatever its password (prefixtaken.go, #262).
	HeldEpochs(ctx context.Context, org, agent string) ([]int32, error)
}

// Repositories gives an agent's repositories and their passwords (checkpoint.Remote): Get the
// current one, which checkpoints are written to, and At the one of a given repository epoch, which
// a checkpoint written there is restored from (ADR 0085 decision 13).
type Repositories interface {
	Get(ctx context.Context, org, agent pgtype.UUID) (checkpoint.Repository, error)
	At(ctx context.Context, org, agent pgtype.UUID, epoch int32) (checkpoint.Repository, error)
}

// Checkpoints turns the writer on. A nil Config.Checkpoints leaves checkpoints off: nothing is
// saved or restored and recreations keep deleting the home, as before.
type Checkpoints struct {
	Store        HomeStore
	Repositories Repositories
	// Interval is the gap between a sandbox's hourly checkpoints (default 1 hour).
	Interval time.Duration
	// Poll is how often the writer looks for requests (default 10 seconds).
	Poll time.Duration
	// Sweep is how often retired repositories past their deletion deadline are deleted (default
	// 10 minutes).
	Sweep time.Duration
	// Lease is how long a running request is left to its writer before another takes it over
	// (default 20 minutes). It must be longer than Timeout.
	Lease time.Duration
	// Timeout bounds one backup or restore attempt (default 15 minutes).
	Timeout time.Duration
	// Attempts is how many times a request is tried (default 3).
	Attempts int
	// Retry is the delay after the first failed attempt, doubled for each further one (default 30 seconds).
	Retry time.Duration
	// SandboxWait is how long a request waits for its sandbox to be ready (default 30 minutes).
	SandboxWait time.Duration
	// Concurrency is how many backups run at once (default 2).
	Concurrency int
	// RestoreConcurrency is how many restores stage a home at once (default 1). A restore stages
	// the whole snapshot on the scratch disk, so the scratch size must cover
	// (Concurrency + RestoreConcurrency) homes of checkpoint.DefaultLimits.MaxBytes.
	RestoreConcurrency int
	// RestoreTries is how many times each checkpoint is tried at restore, newest first, and
	// RestoreFallbacks how many older checkpoints are tried after it (defaults 2 and 2).
	RestoreTries, RestoreFallbacks int

	restoreSlots chan struct{}
}

func (c *Checkpoints) defaults() {
	setDefault(&c.Interval, time.Hour)
	setDefault(&c.Poll, 10*time.Second)
	setDefault(&c.Sweep, 10*time.Minute)
	setDefault(&c.Lease, 20*time.Minute)
	setDefault(&c.Timeout, 15*time.Minute)
	setDefault(&c.Attempts, 3)
	setDefault(&c.Retry, 30*time.Second)
	setDefault(&c.SandboxWait, 30*time.Minute)
	setDefault(&c.Concurrency, 2)
	setDefault(&c.RestoreConcurrency, 1)
	if c.restoreSlots == nil {
		c.restoreSlots = make(chan struct{}, c.RestoreConcurrency)
	}
	setDefault(&c.RestoreTries, 2)
	setDefault(&c.RestoreFallbacks, 2)
}

// tarCommand runs tar through sh so that the sandbox's PATH finds it.
func tarCommand(args ...string) []string {
	return append([]string{"/bin/sh", "-c", `exec tar "$@"`, "tar"}, args...)
}

// checkpointLoop makes the hourly requests and works through the requests until ctx ends.
func (p *Pool) checkpointLoop(ctx context.Context) {
	c := p.cfg.Checkpoints
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, c.Concurrency)
	// Seeds hold at most Concurrency-1 of the backup slots (at least one), so that after keys
	// new-repositories --all the stop and hourly checkpoints still get a writer.
	seeds := make(chan struct{}, max(1, c.Concurrency-1))
	var swept time.Time
	sweeping := make(chan struct{}, 1)
	resyncing := make(chan struct{}, 1)
	probing := make(chan struct{}, 1)
	for ctx.Err() == nil {
		// One tick at a time, so a panic is logged and the hourly checkpoints go on.
		safego.Do(p.log, "checkpoint-tick", func() { p.checkpointTick(ctx, slots, &wg) })
		// Seeds take the backup slots the requests left free (seed.go).
		safego.Do(p.log, "seed-tick", func() { p.seedTick(ctx, slots, seeds, &wg) })
		// Resyncs (resync.go) beside the loop, one pass at a time: listing a bucket of many agents
		// takes a while, and needs no backup slot.
		select {
		case resyncing <- struct{}{}:
			wg.Go(func() {
				defer func() { <-resyncing }()
				safego.Do(p.log, "checkpoint-resync", func() { p.resyncTick(ctx) })
			})
		default:
		}
		// Storage probes of keys commands (probe.go), beside the loop: a keys command waits for
		// the answer, and a slow listing must not delay the ticks nor wait for a backup slot.
		select {
		case probing <- struct{}{}:
			wg.Go(func() {
				defer func() { <-probing }()
				safego.Do(p.log, "repository-probe", func() { p.probeTick(ctx) })
			})
		default:
		}
		if now := time.Now(); now.Sub(swept) >= c.Sweep {
			// Beside the loop: a long sweep (serial blob deletes) must not delay the ticks.
			// A sweep that is still running means this one is skipped.
			select {
			case sweeping <- struct{}{}:
				swept = now
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sweeping }()
					safego.Do(p.log, "repository-sweep", func() { p.sweepRepositories(ctx) })
				}()
			default:
			}
		}
		if !sleep(ctx, c.Poll) {
			return
		}
	}
}

func (p *Pool) checkpointTick(ctx context.Context, slots chan struct{}, wg *sync.WaitGroup) {
	c := p.cfg.Checkpoints
	// Only the orgs with an open request or a sandbox due an hourly one (koinon issue 576).
	orgs, err := p.q.ListOrgIDsWithCheckpointWork(ctx, int32(c.Interval.Seconds()))
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("checkpoints: listing orgs failed", "err", err)
		}
		return
	}
	for _, org := range orgs {
		// Ephemeral agents are skipped by the query (the active version's ephemeral flag).
		if _, err := p.q.RequestHourlyHomeCheckpoints(ctx, store.RequestHourlyHomeCheckpointsParams{
			OrgID: org, IntervalSeconds: int32(c.Interval.Seconds()),
		}); err != nil && ctx.Err() == nil {
			p.log.Warn("checkpoints: requesting hourly checkpoints failed", "err", err)
		}
		wait := int32(c.SandboxWait.Seconds())
		if _, err := p.q.FailStaleHomeCheckpoints(ctx, store.FailStaleHomeCheckpointsParams{
			OrgID: org, WaitSeconds: wait, Error: pgtype.Text{String: fmt.Sprintf("the sandbox was not ready to be read within %s", c.SandboxWait), Valid: true},
		}); err != nil && ctx.Err() == nil {
			p.log.Warn("checkpoints: failing stale requests failed", "err", err)
		}
		// A running request whose writer died is taken over once the sandbox is ready; one whose
		// sandbox stays not ready (a stopped or archived agent) is failed after the same wait.
		if _, err := p.q.FailAbandonedHomeCheckpoints(ctx, store.FailAbandonedHomeCheckpointsParams{
			OrgID: org, LeaseSeconds: int32(c.Lease.Seconds()), WaitSeconds: wait,
			Error: pgtype.Text{String: fmt.Sprintf("the writer stopped and the sandbox was not ready to be read again within %s", c.SandboxWait), Valid: true},
		}); err != nil && ctx.Err() == nil {
			p.log.Warn("checkpoints: failing abandoned requests failed", "err", err)
		}
		for {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			default:
				return // all writers busy
			}
			cp, err := p.q.ClaimHomeCheckpoint(ctx, store.ClaimHomeCheckpointParams{OrgID: org, LeaseSeconds: int32(c.Lease.Seconds())})
			if err != nil {
				<-slots
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					p.log.Warn("checkpoints: claiming a request failed", "err", err)
				}
				break
			}
			wg.Go(func() {
				defer func() { <-slots }()
				// A panic is logged; the request's lease runs out and the request is taken over.
				safego.Do(p.log, "take-checkpoint", func() { p.takeCheckpoint(ctx, cp) })
			})
		}
	}
}

// sweepRepositories deletes the blobs of retired repositories past their deletion deadline and
// marks them deleted (ADR 0085 decision 13); their checkpoints are no longer listed for a restore.
// Several replicas may sweep the same repository: deleting is idempotent, and the mark is set once.
func (p *Pool) sweepRepositories(ctx context.Context) {
	orgs, err := p.q.ListOrgIDsWithRepositoryDeleteWork(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("repository sweep: listing orgs failed", "err", err)
		}
		return
	}
	homes := p.cfg.Checkpoints.Store
	refused, complete := 0, true
	defer func() {
		if complete {
			p.cfg.Metrics.SweepRefused.Set(float64(refused))
		}
	}()
	for _, org := range orgs {
		due, err := p.q.ListDueHomeRepositories(ctx, store.ListDueHomeRepositoriesParams{
			OrgID: org, LeaseSeconds: int32(p.cfg.Checkpoints.Lease.Seconds()), MaxRows: 50,
		})
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("repository sweep: listing due repositories failed", "err", err)
			}
			complete = false
			continue
		}
		for _, r := range due {
			log := p.log.With("agent", r.AgentID.String(), "repository_epoch", r.RepositoryEpoch)
			// The prefix is recomputed, never taken from the row: a row that names another agent's
			// or org's folder is refused, so the sweep deletes only this agent's own repository.
			want := checkpoint.EpochPrefix(org.String(), r.AgentID.String(), r.RepositoryEpoch)
			key := org.String() + "/" + checkpoint.EpochRow(r.AgentID.String(), r.RepositoryEpoch)
			if r.Prefix != want {
				// Logged once per process (until the row is fixed); the gauge counts it on every
				// sweep, for an alert.
				refused++
				if _, seen := p.sweepRefused.LoadOrStore(key, true); !seen {
					log.Error("repository sweep: a retired repository's stored prefix is not its own; not deleting it (logged once; see koinon_agentd_home_repository_sweep_refused)", "prefix", r.Prefix, "want", want)
				}
				continue
			}
			p.sweepRefused.Delete(key) // fixed: a later refusal of the row is logged again
			n := 0
			if !r.PrefixTaken {
				n, err = homes.DeleteRepository(ctx, want)
				if err != nil {
					if ctx.Err() == nil {
						log.Warn("repository sweep: deleting a retired repository failed; trying again later", "err", err)
					}
					continue
				}
			} // else: the prefix holds another repository, not the row's (prefixtaken.go); its blobs stay
			if r.RepositoryEpoch == 0 {
				_, err = p.q.MarkHomeRepositoryDeleted(ctx, store.MarkHomeRepositoryDeletedParams{OrgID: org, AgentID: r.AgentID})
			} else {
				_, err = p.q.MarkHomeRepositoryEpochDeleted(ctx, store.MarkHomeRepositoryEpochDeletedParams{OrgID: org, AgentID: r.AgentID, RepositoryEpoch: r.RepositoryEpoch})
			}
			if err != nil {
				log.Warn("repository sweep: marking a repository deleted failed", "err", err)
				continue
			}
			log.Info("retired home repository deleted", "blobs", n)
		}
	}
}

// takeCheckpoint backs up one claimed request: it reads the home over exec into the writer and
// records the outcome.
func (p *Pool) takeCheckpoint(ctx context.Context, cp store.HomeCheckpoint) {
	c := p.cfg.Checkpoints
	log := p.log.With("agent", cp.AgentID.String(), "checkpoint", cp.ID.String(), "reason", cp.Reason, "attempt", cp.Attempts)
	// Timeout bounds the whole attempt from here: the lookups before the exec (the database, and the
	// repository password, which may call a KMS), the transfer and the record of the result. Timeout is
	// shorter than Lease, so a writer ends within its lease and FailAbandonedHomeCheckpoints and the
	// sweeper never act on a row whose writer still writes.
	tctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	res, err := p.backupHome(tctx, cp)
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and another replica takes the request over
	}
	// The outcome is recorded on its own bounded context, not on tctx (which may have just expired,
	// and would then fail the record of a snapshot that exists) and not on ctx alone (a stalled
	// database would keep the writer's slot past its lease). It keeps the attempt's deadline plus
	// outcomeBound (WithoutCancel drops the deadline, so it is set again) and still ends at shutdown;
	// Timeout + outcomeBound is shorter than Lease (checked in New).
	attemptDeadline, _ := tctx.Deadline()
	octx, ocancel := context.WithDeadline(context.WithoutCancel(tctx), attemptDeadline.Add(outcomeBound))
	defer ocancel()
	stop := context.AfterFunc(ctx, ocancel)
	defer stop()
	if err == nil {
		log.Info("home checkpoint complete", "bytes", res.Bytes, "files", res.Files)
		_, cerr := p.q.CompleteHomeCheckpoint(octx, store.CompleteHomeCheckpointParams{
			OrgID: cp.OrgID, ID: cp.ID, SnapshotID: pgtype.Text{String: res.SnapshotID, Valid: true}, SizeBytes: pgtype.Int8{Int64: res.Bytes, Valid: true},
			Epoch: res.epoch, Generation: res.generation, RepositoryEpoch: res.repositoryEpoch,
		})
		if cerr == nil {
			return
		}
		// The snapshot exists but the row still says running. Release it for another attempt (or fail
		// it after the last one) instead of leaving it to the lease.
		log.Warn("recording a complete checkpoint failed", "err", cerr)
		if ctx.Err() != nil {
			return // shutting down: the lease runs out and another replica takes the request over
		}
		err = fmt.Errorf("recording the complete checkpoint: %w", cerr)
	}
	msg := truncateText(err.Error(), 400)
	var perm *permanent
	if errors.As(err, &perm) || int(cp.Attempts) >= c.Attempts {
		log.Warn("home checkpoint failed", "err", msg)
		if _, err := p.q.FailHomeCheckpoint(octx, store.FailHomeCheckpointParams{OrgID: cp.OrgID, ID: cp.ID, Error: pgtype.Text{String: msg, Valid: true}}); err != nil {
			log.Warn("recording a failed checkpoint failed", "err", err)
		}
		return
	}
	delay := c.Retry << (cp.Attempts - 1)
	log.Warn("home checkpoint attempt failed; trying again", "err", msg, "in", delay)
	if _, err := p.q.ReleaseHomeCheckpoint(octx, store.ReleaseHomeCheckpointParams{
		OrgID: cp.OrgID, ID: cp.ID, Error: pgtype.Text{String: msg, Valid: true}, DelaySeconds: int32(delay.Seconds()),
	}); err != nil {
		log.Warn("releasing a checkpoint failed", "err", err)
	}
}

// outcomeBound bounds the record of an attempt's outcome (complete, fail or release) after the
// attempt's own deadline. Checkpoints.Timeout + outcomeBound must stay shorter than Lease.
const outcomeBound = 30 * time.Second

// permanent marks an error that another attempt cannot fix.
type permanent struct{ err error }

func (e *permanent) Error() string { return e.err.Error() }
func (e *permanent) Unwrap() error { return e.err }

type backupResult struct {
	checkpoint.Result
	epoch      int64
	generation string
	// repositoryEpoch is the repository the snapshot was written to.
	repositoryEpoch int32
}

// backupHome runs under the attempt's Timeout (takeCheckpoint sets it on ctx).
func (p *Pool) backupHome(ctx context.Context, cp store.HomeCheckpoint) (backupResult, error) {
	c := p.cfg.Checkpoints
	sb, err := p.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: cp.OrgID, AgentID: cp.AgentID})
	if err != nil {
		return backupResult{}, err
	}
	switch {
	case sb.RestoreCheckpointID.Valid:
		// The home has not been restored into this sandbox yet: its contents are not the agent's.
		return backupResult{}, &permanent{errors.New("the sandbox is waiting for its home to be restored")}
	case sb.State != store.SandboxStateReady:
		return backupResult{}, errors.New("the sandbox is not ready")
	case cp.SandboxID != "" && sb.SandboxID.String != cp.SandboxID:
		return backupResult{}, &permanent{errors.New("the sandbox was replaced after the checkpoint was requested")}
	}
	repo, err := c.Repositories.Get(ctx, cp.OrgID, cp.AgentID)
	if err != nil {
		return backupResult{}, err
	}
	epoch, err := p.q.MaxAgentEpoch(ctx, store.MaxAgentEpochParams{OrgID: cp.OrgID, AgentID: cp.AgentID})
	if err != nil {
		return backupResult{}, err
	}
	generation := sb.HomeGeneration.String

	var stderr tail
	proc, err := p.backend.Exec(ctx, sb.Workspace, sb.SandboxName, execution.ExecRequest{
		Command: tarCommand(append([]string{"-C", checkpoint.HomeDir}, append(checkpoint.TarExcludeArgs(), "--hard-dereference", "-cf", "-", ".")...)...),
		Stderr:  &stderr, Timeout: c.Timeout,
	})
	if err != nil {
		return backupResult{}, fmt.Errorf("reading the home: %w", err)
	}
	defer func() { _ = proc.Close() }()
	_ = proc.Stdin().Close()
	// The home is checked before the snapshot is written, not after: a snapshot of a home tar did not
	// read whole would sit in the bucket, and checkpoints resync records what the bucket holds.
	var readErr error
	ready := func() error {
		// The tar stream ends with padding after its last entry; read it so the command can exit.
		_, _ = io.Copy(io.Discard, proc.Stdout())
		// GNU tar exits 1 when a file changed while it was read: the home is live.
		if code, err := proc.Wait(); err != nil {
			readErr = fmt.Errorf("reading the home: %w", err)
		} else if code > 1 {
			readErr = fmt.Errorf("tar in the sandbox exited with %d: %s", code, strings.TrimSpace(stderr.String()))
		}
		return readErr
	}
	res, err := c.Store.Backup(ctx, repo.Prefix, repo.Password, proc.Stdout(), checkpoint.Meta{
		CheckpointID: cp.ID.String(), Generation: generation, Epoch: epoch,
		Reason: string(cp.Reason), Sandbox: sb.SandboxID.String, Ready: ready,
	})
	if err != nil {
		if checkpoint.TakenPrefix(err, repo.Prefix) {
			return backupResult{}, p.prefixTaken(ctx, cp.OrgID, cp.AgentID, repo, err)
		}
		if sctx := stderr.String(); sctx != "" && readErr == nil {
			err = fmt.Errorf("%w (tar: %s)", err, strings.TrimSpace(sctx))
		}
		return backupResult{}, err
	}
	if cp.Reason == store.CheckpointReasonRecreate {
		if err := c.Store.Verify(ctx, repo.Prefix, repo.Password, res.SnapshotID); err != nil {
			return backupResult{}, fmt.Errorf("verifying the snapshot: %w", err)
		}
	}
	return backupResult{Result: res, epoch: epoch, generation: generation, repositoryEpoch: repo.Epoch}, nil
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

// restoreHome restores the sandbox's home from a checkpoint when the reconciler recreated the
// sandbox from one (agent_sandboxes.restore_checkpoint_id). It runs before the harness starts,
// so the session files are in place for session/load. One run restores at a time; the others
// wait for it. The newest checkpoint is tried first and, when it will not restore, older ones.
func (r *runner) restoreHome(ctx context.Context, sb store.AgentSandbox) error {
	c := r.p.cfg.Checkpoints
	if c == nil || !sb.RestoreCheckpointID.Valid {
		return nil
	}
	org, agent := r.run.OrgID, r.run.AgentID
	if agentconfig.IsEphemeral(r.vcfg) {
		// An ephemeral agent's task sandbox starts empty: nothing carries over between tasks (ADR 0065
		// 13c), and the newest checkpoint of the agent may belong to another task.
		return r.q.ClearSandboxRestore(ctx, store.ClearSandboxRestoreParams{OrgID: org, AgentID: agent})
	}
	// One restore can take Timeout for each try of each checkpoint; the claim lasts as long as the
	// run is willing to wait for it, so a second run never takes it over while the first extracts.
	budget := c.Timeout * time.Duration(c.RestoreTries*(1+c.RestoreFallbacks)+1)
	deadline := r.p.cfg.Now().Add(budget)
	for {
		_, err := r.q.ClaimSandboxRestore(ctx, store.ClaimSandboxRestoreParams{OrgID: org, AgentID: agent, LeaseSeconds: int32(budget.Seconds())})
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Done, or another run is restoring.
		cur, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org, AgentID: agent})
		if err != nil {
			return err
		}
		if !cur.RestoreCheckpointID.Valid {
			return nil
		}
		if r.p.cfg.Now().After(deadline) {
			return &failure{reason: "another run's restore of the home did not finish", class: classTransient}
		}
		if !sleep(ctx, r.p.cfg.Poll) {
			return ctx.Err()
		}
	}
	err := r.restoreFromCheckpoints(ctx, sb)
	if err != nil {
		if rerr := r.q.ReleaseSandboxRestore(context.WithoutCancel(ctx), store.ReleaseSandboxRestoreParams{OrgID: org, AgentID: agent}); rerr != nil {
			r.log.Warn("releasing the home restore failed", "err", rerr)
		}
		return err
	}
	return r.q.ClearSandboxRestore(ctx, store.ClearSandboxRestoreParams{OrgID: org, AgentID: agent})
}

func (r *runner) restoreFromCheckpoints(ctx context.Context, sb store.AgentSandbox) error {
	c := r.p.cfg.Checkpoints
	org, agent := r.run.OrgID, r.run.AgentID
	cps, err := r.q.ListCompleteHomeCheckpoints(ctx, store.ListCompleteHomeCheckpointsParams{OrgID: org, AgentID: agent, MaxRows: int32(1 + c.RestoreFallbacks)}) //nolint:gosec // small
	if err != nil {
		return err
	}
	if len(cps) == 0 {
		return &failure{reason: "restoring the home: the agent has no complete checkpoint, so the new sandbox's home is left empty and the run is not started"}
	}
	// Each checkpoint is restored from the repository it was written to; a later repository epoch
	// does not hold the checkpoints written before it started.
	repos := map[int32]checkpoint.Repository{}
	var errs []error
	for i, cp := range cps {
		repo, ok := repos[cp.RepositoryEpoch]
		if !ok {
			var err error
			repo, err = c.Repositories.At(ctx, org, agent, cp.RepositoryEpoch)
			if errors.Is(err, checkpoint.ErrNoRepository) || errors.Is(err, checkpoint.ErrRepositoryLost) {
				// Nothing to open it with: an older checkpoint, in another epoch, may still restore.
				r.log.Warn("restoring the home: the checkpoint's repository cannot be opened", "checkpoint", cp.ID.String(), "repository_epoch", cp.RepositoryEpoch, "err", err)
				errs = append(errs, fmt.Errorf("checkpoint %s (repository epoch %d): %w", cp.ID, cp.RepositoryEpoch, err))
				continue
			}
			if err != nil {
				return &failure{reason: "restoring the home: " + err.Error(), class: classTransient}
			}
			repos[cp.RepositoryEpoch] = repo
		}
		for try := 1; try <= c.RestoreTries; try++ {
			err := r.restoreOne(ctx, sb, repo, cp)
			if err == nil {
				if recErr := r.own.Record(ctx, "run.home_restored", map[string]any{
					"checkpoint": cp.ID.String(), "snapshot": cp.SnapshotID.String, "newer_failed": i,
					"taken_at": cp.FinishedAt.Time.UTC().Format(time.RFC3339),
				}); recErr != nil {
					r.log.Warn("recording the home restore failed", "err", recErr)
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, execution.ErrUnsupported) {
				// The sandbox's supervisor cannot take an upload: another try or an older
				// checkpoint would fail the same way.
				return &failure{reason: "restoring the home: " + truncateText(err.Error(), 600), class: classTransient}
			}
			r.log.Warn("restoring the home failed", "checkpoint", cp.ID.String(), "try", try, "err", err)
			errs = append(errs, fmt.Errorf("checkpoint %s: %w", cp.ID, err))
			if try < c.RestoreTries && !sleep(ctx, time.Second<<try) {
				return ctx.Err()
			}
		}
	}
	return &failure{reason: "restoring the home failed: " + truncateText(errors.Join(errs...).Error(), 600), class: classTransient}
}

// restoreOne streams one checkpoint into the sandbox's home through the execution layer's upload
// channel. Exec stdin is not used: it has no flow control, and the supervisor kills a command
// whose unread stdin passes 4 MiB (ADR 0032, 2026-10-27, issue 169). The archive is shaped by
// checkpoint.WriteTar for the upload's fixed `tar xf - -C /sandbox`.
func (r *runner) restoreOne(ctx context.Context, sb store.AgentSandbox, repo checkpoint.Repository, cp store.HomeCheckpoint) error {
	c := r.p.cfg.Checkpoints
	// The restore stages the snapshot on the scratch disk: only so many at once.
	select {
	case c.restoreSlots <- struct{}{}:
		defer func() { <-c.restoreSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	actx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	// The writer produces the tar and the upload consumes it. If the upload ends first, closing the
	// read side stops the writer; if the writer fails, the upload reads its error and leaves the
	// archive unterminated, so the sandbox's tar never sees a complete one.
	pr, pw := io.Pipe()
	restored := make(chan error, 1)
	go func() {
		err := c.Store.Restore(actx, repo.Prefix, repo.Password, cp.SnapshotID.String, pw)
		_ = pw.CloseWithError(err) // nil closes with io.EOF
		restored <- err
	}()
	uerr := r.p.backend.Upload(actx, sb.Workspace, sb.SandboxName, checkpoint.HomeDir, pr)
	if uerr == nil {
		// The upload read an end-of-file, so the writer is done.
		return <-restored
	}
	// The upload failed first (an old supervisor, a host key mismatch, an expired token, a
	// connection limit): stop the writer instead of waiting for it. kopia stages the whole
	// snapshot before it writes the first byte.
	_ = pr.CloseWithError(uerr)
	cancel()
	if rerr := <-restored; rerr != nil && !errors.Is(rerr, uerr) {
		r.log.Debug("the home writer ended after the upload failed", "err", rerr)
	}
	// If the writer failed first, the upload read its error and uerr wraps it; otherwise the
	// writer's error is only the cancellation we caused.
	return uerr
}
