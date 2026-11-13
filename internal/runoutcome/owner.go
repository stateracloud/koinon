// SPDX-License-Identifier: Apache-2.0

// Package runoutcome owns how a run ends on its timeline (the ADR "One writer ends a run").
//
// A worker that claimed a run opens an Owner for it. The Owner holds the run's org, id, epoch, the
// source seq of its events and the fence state, and it is the only writer of the run's closing rows:
// the closing run event, FinishRun, the reply and the park note on the task, the task's end and the
// end of the run's trace turn row. Every verb writes its rows in one transaction in the order the
// timeline needs:
//
//  1. the trace turn row ends before the run row changes, so whoever sees the run finished sees the
//     turn ended;
//  2. the agent's reply comes before the park note and before run.parked, after which the reply
//     starts afresh;
//  3. the task ends after the reply (ADR 0065);
//  4. the source seq is only incremented under the Owner's mutex.
//
// A write by a stale epoch matches no row and writes nothing (ADR 0030): the verb fences the Owner
// and returns ErrFenced.
package runoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// ErrFenced: the worker may no longer write (another owner, a cancel, or it gave up).
var ErrFenced = errors.New("no longer the run's owner")

// Fence says why a worker stopped being allowed to write.
type Fence int32

const (
	// FenceNone: the Owner is not fenced.
	FenceNone Fence = iota
	// FenceLost: a newer epoch owns the run, or it finished.
	FenceLost
	// FenceCanceled: the control plane canceled the run.
	FenceCanceled
	// FenceAbandon: the worker cannot reach the database and gives up.
	FenceAbandon
	// FenceFailed: the worker must end the run as failed, with a reason it holds, because the cause
	// is permanent and a takeover would meet it again (the leak guard's feed, koinon#123). The
	// ending verbs still write; the turn row ends with the run.
	FenceFailed
)

// DB begins the transactions of the verbs; *pgxpool.Pool is one.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RunStep is a step that runs in the transaction in which a run ends, after the run and its task
// events are written. An error rolls the whole end back.
type RunStep func(ctx context.Context, q *store.Queries, run store.Run) error

// Metrics counts what the verbs did. A nil Metrics counts nothing.
type Metrics interface {
	// Finished counts a run that ended, parked or succeeded, by state.
	Finished(state store.RunState)
	Requeued()
	// SandboxWaitCap counts a run failed because its sandbox never became ready.
	SandboxWaitCap(cause execution.SandboxWaitCause)
	// EventWriteFailed counts a run event Record could not write after its tries, by the event's
	// class and why. A diagnostic one was dropped; a business one abandoned the run.
	EventWriteFailed(class EventClass, reason WriteFailure)
}

// Config is what an Owner needs.
type Config struct {
	DB      DB
	Queries *store.Queries
	// Run is the run as the worker claimed it: org, id, epoch, task and agent.
	Run store.Run
	Log *slog.Logger
	// Now is the clock of the park's default expiry; nil is time.Now.
	Now     func() time.Time
	Metrics Metrics
	// AfterTimeLimit, when set, runs in the transaction that cancels the run at its wall-clock cap
	// (CancelAtTimeLimit). agentd wires scheduler.ContinueAfterTimeLimit: it queues a continuation
	// run, or blocks the task on its owner (issue #99). Nil does nothing.
	AfterTimeLimit RunStep
	// Stop reports a reason, besides the fence, why the worker must not write (the pool is shutting
	// down). It is asked before a database error is read as a fence, and its error is returned as it
	// is. Nil means there is none.
	Stop func(ctx context.Context) error
	// Live receives every text chunk of a message or thought as it arrives (kind "acp.<update>",
	// the update's JSON). Chunks are never stored, only the items they make up (items.go); this is
	// how a live subscriber sees them. It runs on the harness's notification path, so it must not
	// block. Nil means none.
	// agentd wires it to a runwatch.ChunkPublisher, which batches the chunks into koinon_run
	// notifications for the API's WatchRun streams.
	// item names the item the chunk belongs to; the run event stored for that item carries the same
	// name in its payload as "item" (items.go).
	Live func(kind, item string, payload []byte)
	// LiveFlush sends what Live has queued and returns when it is sent. The Owner calls it before it
	// stores an item, so the item's live tail is delivered ahead of the event that replaces it. Nil
	// means none.
	LiveFlush func()
}

// Owner is the writer of one run under one epoch. It is safe for concurrent use.
type Owner struct {
	cfg Config
	run store.Run
	q   *store.Queries

	mu      sync.Mutex // serialises appends, srcSeq and pend
	srcSeq  int64
	itemSeq int64   // the last item number handed out (items.go)
	pend    pending // the chunks of the item in progress (items.go)

	fenceOnce sync.Once
	fenced    chan struct{}
	fenceKind atomic.Int32
}

// New opens the Owner of a claimed run.
func New(cfg Config) *Owner {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Owner{cfg: cfg, run: cfg.Run, q: cfg.Queries, fenced: make(chan struct{})}
}

// Fenced is closed once the Owner may no longer write.
func (o *Owner) Fenced() <-chan struct{} { return o.fenced }

// IsFenced reports whether the Owner may no longer write.
func (o *Owner) IsFenced() bool {
	select {
	case <-o.fenced:
		return true
	default:
		return false
	}
}

// FenceKind is why the Owner was fenced; FenceNone before.
func (o *Owner) FenceKind() Fence { return Fence(o.fenceKind.Load()) }

// Fence stops the Owner from writing. Only the first reason counts.
func (o *Owner) Fence(k Fence) {
	o.fenceOnce.Do(func() {
		o.fenceKind.Store(int32(k))
		close(o.fenced)
	})
}

// FenceFromDB fences after a write matched no rows, telling a cancel from a lost epoch.
func (o *Owner) FenceFromDB(ctx context.Context) {
	cur, err := o.q.GetRun(ctx, store.GetRunParams{OrgID: o.run.OrgID, ID: o.run.ID})
	switch {
	case err != nil:
		o.Fence(FenceAbandon)
	case cur.State == store.RunStateCanceled && cur.Epoch == o.run.Epoch:
		o.Fence(FenceCanceled)
	default:
		o.Fence(FenceLost)
	}
}

// Err classifies a database error of the run's own writes: the reason to stop writing, a fence
// (no row matched) or the error with what failed.
func (o *Owner) Err(ctx context.Context, what string, err error) error {
	if o.cfg.Stop != nil {
		if stop := o.cfg.Stop(ctx); stop != nil {
			return stop
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		o.FenceFromDB(ctx)
		return ErrFenced
	}
	return fmt.Errorf("%s: %w", what, err)
}

// failed is Err for an ending verb whose transaction rolled back, turn row included. Unless another
// owner took the run over or the worker gave it up, the turn row is ended on its own, best effort, so a
// run canceled meanwhile does not keep a turn that looks like it is still running.
func (o *Owner) failed(ctx context.Context, turn Turn, what string, err error) error {
	err = o.Err(ctx, what, err)
	if k := o.FenceKind(); k != FenceLost && k != FenceAbandon {
		o.CloseTurn(ctx, turn)
	}
	return err
}

// Record appends a run event under the next source seq, retrying transient failures. The source seq
// makes a retry after an ambiguous failure (the insert committed, the reply was lost) a no-op
// instead of a duplicate. What a write that keeps failing does depends on the kind's class
// (ClassOf): a business event abandons the run rather than drop it (the lease expires and another
// worker resumes the session); a diagnostic event is dropped, logged and counted after at most two
// tries, and Record returns nil. Either class fences when the write matches no row: the run is no
// longer this worker's.
func (o *Owner) Record(ctx context.Context, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return o.RecordRaw(ctx, kind, b)
}

// RecordRaw is Record for a payload that is JSON already.
func (o *Owner) RecordRaw(ctx context.Context, kind string, payload []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.IsFenced() {
		return ErrFenced
	}
	// The item in progress comes first on the timeline (items.go).
	if err := o.flushLocked(ctx, false, false); err != nil {
		return err
	}
	return o.recordLocked(ctx, kind, payload, false)
}

// recordLocked is RecordRaw with o.mu held and the Owner not fenced. cutOff is true only for the
// item FlushItems stores after a cancel.
func (o *Owner) recordLocked(ctx context.Context, kind string, payload []byte, cutOff bool) error {
	o.srcSeq++
	src := o.srcSeq
	class := ClassOf(kind)
	tries := businessTries
	if class == ClassDiagnostic {
		tries = diagnosticTries
	}
	backoff := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := o.tryAppend(ctx, class, kind, payload, src, cutOff)
		if err == nil {
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			o.FenceFromDB(ctx)
			return ErrFenced
		}
		if attempt == tries || !sleep(ctx, backoff) {
			reason := failureOf(err)
			if o.cfg.Metrics != nil {
				o.cfg.Metrics.EventWriteFailed(class, reason)
			}
			if class == ClassDiagnostic {
				o.cfg.Log.Warn("dropped a diagnostic run event that could not be recorded", "kind", kind, "reason", reason, "err", err)
				return nil
			}
			o.cfg.Log.Warn("recording a run event failed", "kind", kind, "reason", reason, "err", err)
			o.Fence(FenceAbandon)
			return err
		}
		backoff *= 2
	}
}

// tryAppend is one try of RecordRaw; the caller holds o.mu. It returns nil when the event is
// stored, by this try or by an earlier one whose reply was lost: the insert matched no row
// (pgx.ErrNoRows) or hit a unique violation, and the event is there under its source seq. A
// diagnostic try has its own short deadline, so a hung connection holds o.mu, and with it the
// run's other writes, for at most diagnosticTryTimeout; a business try runs under ctx alone.
func (o *Owner) tryAppend(ctx context.Context, class EventClass, kind string, payload []byte, src int64, cutOff bool) error {
	if class == ClassDiagnostic {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, diagnosticTryTimeout)
		defer cancel()
	}
	var err error
	if cutOff {
		// Only the item FlushItems stores after a cancel takes this path, and the caller says so:
		// the fence is never read here, because another goroutine can set it between a caller's
		// check and this write, and an event that raced the cancel must be refused, not written.
		_, err = o.q.AppendCanceledRunEventOnce(ctx, store.AppendCanceledRunEventOnceParams{
			OrgID: o.run.OrgID, RunID: o.run.ID, Epoch: o.run.Epoch,
			Kind: kind, Payload: payload, SourceSeq: src,
		})
	} else {
		_, err = o.q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: o.run.OrgID, RunID: o.run.ID, Epoch: o.run.Epoch,
			Kind: kind, Payload: payload, SourceSeq: src,
		})
	}
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if _, gerr := o.q.GetRunEventBySource(ctx, store.GetRunEventBySourceParams{
			OrgID: o.run.OrgID, RunID: o.run.ID, Epoch: o.run.Epoch, SourceSeq: src,
		}); gerr == nil {
			return nil // an earlier attempt committed
		}
	}
	return err
}

// The tries of Record before it gives up. A business event keeps trying for about 1.5 s plus the
// time its tries take under the caller's context. A diagnostic one tries twice, 100 ms apart, each
// try with its own diagnosticTryTimeout, so it holds the run up for about 4 s at the most.
const (
	businessTries   = 5
	diagnosticTries = 2
)

// diagnosticTryTimeout is the deadline of one try of a diagnostic event's write; a variable for tests.
var diagnosticTryTimeout = 2 * time.Second

// sleep waits for d or until ctx ends, and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Turn is the run's open trace turn row (ADR 0074), as an input to the verbs that end the run or
// its turn. The zero Turn is none.
type Turn struct {
	// ID is the row; invalid means no row to end.
	ID pgtype.UUID
	// Reason is how the turn ended, for the verbs that do not know it themselves (EndTurn does).
	Reason string
}

// endRow ends the turn row in the caller's transaction. A row that is ended already, or gone, is
// not an error.
func (o *Owner) endRow(ctx context.Context, q *store.Queries, t Turn, reason string) error {
	if !t.ID.Valid {
		return nil
	}
	_, err := q.EndRunTurn(ctx, store.EndRunTurnParams{
		OrgID: o.run.OrgID, ID: t.ID, EndReason: pgtype.Text{String: reason, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// CloseTurn ends the turn row of a run whose state it does not change: a run that was canceled
// elsewhere, or is handed back. The write is best effort, like the trace it belongs to.
func (o *Owner) CloseTurn(ctx context.Context, t Turn) {
	if !t.ID.Valid {
		return
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := o.endRow(ectx, o.q, t, t.Reason); err != nil {
		o.cfg.Log.Warn("ending the turn row failed", "err", err)
	}
}

// nextSrc takes the next source seq; the caller holds o.mu.
func (o *Owner) nextSrc() int64 {
	o.srcSeq++
	return o.srcSeq
}

func (o *Owner) finished(state store.RunState) {
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.Finished(state)
	}
}
