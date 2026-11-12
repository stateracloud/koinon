// SPDX-License-Identifier: Apache-2.0

package properties_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/testing/propseed"
)

// taskWorld drives one task the way the API (messages), the scheduler (Enqueue) and agentd
// (claim, prompt, takeover, finish) do, against the real queries.
type taskWorld struct {
	ctx        context.Context
	org, user  pgtype.UUID
	task       store.Task
	cur        *store.Run // the run an agentd owns now, if any
	mu         sync.Mutex
	sent       int            // messages appended
	delivered  map[string]int // message text -> times it was put in a prompt
	totalDeliv int
	claims     int
	runs       map[string]bool // run IDs that ever existed
}

func newTaskWorld(ctx context.Context) *taskWorld {
	GinkgoHelper()
	org, user := newOrg(ctx)
	agent := newAgent(ctx, org, user)
	return &taskWorld{ctx: ctx, org: org, user: user, task: newTask(ctx, org, user, agent), delivered: map[string]int{}, runs: map[string]bool{}}
}

func (w *taskWorld) queuedRuns() int {
	GinkgoHelper()
	var n int
	Expect(pool.QueryRow(w.ctx, `SELECT count(*) FROM runs WHERE org_id = $1 AND task_id = $2 AND state = 'queued'`, w.org, w.task.ID).Scan(&n)).To(Succeed())
	return n
}

// sendTx appends one user message and enqueues a run, in one transaction (the lock on the task
// row serialises concurrent senders). It is safe to call from several goroutines and returns
// errors instead of asserting, so a goroutine never hides a failure from the seed report.
func (w *taskWorld) sendTx() (queued bool, err error) {
	w.mu.Lock()
	w.sent++
	text := fmt.Sprintf("m%d", w.sent)
	w.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"text": text})
	err = pgx.BeginFunc(w.ctx, pool, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if _, err := tq.AppendTaskEvent(w.ctx, store.AppendTaskEventParams{
			OrgID: w.org, TaskID: w.task.ID, Kind: "message", ActorKind: store.TaskActorKindUser, ActorUserID: w.user, Payload: payload,
		}); err != nil {
			return err
		}
		run, ok, err := scheduler.Enqueue(w.ctx, tq, w.task)
		queued = ok
		if ok {
			w.mu.Lock()
			w.runs[uuidString(run.ID)] = true
			w.mu.Unlock()
		}
		return err
	})
	return queued, err
}

// burst sends n messages, one after the other or from n goroutines, and returns how many of them
// queued a run.
func (w *taskWorld) burst(n int, concurrent bool) int {
	GinkgoHelper()
	var (
		wg      sync.WaitGroup
		queued  int32
		errsMu  sync.Mutex
		errs    []error
		sendOne = func() {
			ok, err := w.sendTx()
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
			if ok {
				atomic.AddInt32(&queued, 1)
			}
		}
	)
	for i := 0; i < n; i++ {
		if !concurrent {
			sendOne()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendOne()
		}()
	}
	wg.Wait()
	Expect(errs).To(BeEmpty())
	return int(queued)
}

func uuidString(u pgtype.UUID) string { return fmt.Sprintf("%x", u.Bytes) }

// claim is an agentd claiming the next run and marking it running; the prompt is a separate step
// (agentd builds it later), so a restart can land before it.
func (w *taskWorld) claim() bool {
	GinkgoHelper()
	run, err := q.ClaimNextRun(w.ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "a", LeaseSeconds: 60})
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	run, err = q.MarkRunRunning(w.ctx, store.MarkRunRunningParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	w.cur = &run
	w.claims++
	return true
}

// prompt builds the run's prompt as agentd.prompt does: from what the task's other runs did not
// send, or from where this run's own prompt ended when it was resumed. The prompt must hold every
// message nobody has sent yet, which is what makes a burst one wake-up.
func (w *taskWorld) prompt() {
	GinkgoHelper()
	run := *w.cur
	after := run.PromptThroughSeq.Int64
	if !run.PromptThroughSeq.Valid {
		var err error
		after, err = q.GetTaskPromptedThrough(w.ctx, store.GetTaskPromptedThroughParams{OrgID: w.org, TaskID: w.task.ID, RunID: run.ID})
		Expect(err).NotTo(HaveOccurred())
	}
	msgs, err := q.ListTaskMessagesAfter(w.ctx, store.ListTaskMessagesAfterParams{OrgID: w.org, TaskID: w.task.ID, AfterSeq: after})
	Expect(err).NotTo(HaveOccurred())
	Expect(msgs).To(HaveLen(w.sent-w.totalDeliv), "a prompt must hold exactly the messages not sent before")
	through := after
	for _, m := range msgs {
		var b struct{ Text string }
		Expect(json.Unmarshal(m.Payload, &b)).To(Succeed())
		w.delivered[b.Text]++
		w.totalDeliv++
		through = m.Seq
	}
	updated, err := q.SetRunPromptThrough(w.ctx, store.SetRunPromptThroughParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch, PromptThroughSeq: through})
	Expect(err).NotTo(HaveOccurred())
	w.cur = &updated
}

// restart kills the owner: another agentd takes the run over under a new epoch. The old owner is
// a zombie: nothing it writes with its old epoch may change the run.
func (w *taskWorld) restart() {
	GinkgoHelper()
	old := *w.cur
	_, err := q.ReleaseRunLease(w.ctx, store.ReleaseRunLeaseParams{OrgID: w.org, ID: old.ID, Epoch: old.Epoch})
	Expect(err).NotTo(HaveOccurred())
	taken, err := q.TakeOverExpiredRun(w.ctx, store.TakeOverExpiredRunParams{OrgID: w.org, OwnerID: "b", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	Expect(taken.ID).To(Equal(old.ID))
	Expect(taken.Epoch).To(BeNumerically(">", old.Epoch))
	expectFenced(w.ctx, w.org, old)
	w.cur = &taken
}

// expectFenced checks that a stale owner's writes match no row and leave the run as it is.
func expectFenced(ctx context.Context, org pgtype.UUID, old store.Run) {
	GinkgoHelper()
	before, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: old.ID})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.FinishRun(ctx, store.FinishRunParams{OrgID: org, ID: old.ID, Epoch: old.Epoch, Outcome: store.RunStateFailed})
	Expect(err).To(MatchError(pgx.ErrNoRows), "a stale owner finished the run")
	_, err = q.ParkRun(ctx, store.ParkRunParams{OrgID: org, ID: old.ID, Epoch: old.Epoch, ParkReason: store.RunParkReasonInputRequired})
	Expect(err).To(MatchError(pgx.ErrNoRows), "a stale owner parked the run")
	_, err = q.SetRunPromptThrough(ctx, store.SetRunPromptThroughParams{OrgID: org, ID: old.ID, Epoch: old.Epoch, PromptThroughSeq: 1 << 40})
	Expect(err).To(MatchError(pgx.ErrNoRows), "a stale owner moved the prompt window")
	after, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: old.ID})
	Expect(err).NotTo(HaveOccurred())
	Expect(after).To(Equal(before))
}

func (w *taskWorld) finish() {
	GinkgoHelper()
	run := *w.cur
	_, err := q.FinishRun(w.ctx, store.FinishRunParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateSucceeded})
	Expect(err).NotTo(HaveOccurred())
	w.cur = nil
}

var _ = Describe("batching (issue 26)", func() {
	// Property: for any interleaving of message bursts (sequential or racing), runs starting,
	// prompts, owner restarts (before or after the prompt) and runs ending, no message is lost or
	// put in a prompt twice, a prompt holds every message not sent before, and a burst queues one
	// run when none waits and none when one does.
	It("never loses or duplicates an event", func(ctx SpecContext) {
		propseed.ForEach(func(rng *rand.Rand) {
			w := newTaskWorld(ctx)
			for step := 0; step < 30+rng.Intn(40); step++ {
				switch op := rng.Intn(12); {
				case op < 5:
					before := w.queuedRuns()
					n := 1 + rng.Intn(5)
					got := w.burst(n, rng.Intn(2) == 0)
					want := 0
					if before == 0 {
						want = 1
					}
					Expect(got).To(Equal(want), "a burst of %d messages with %d runs waiting queued %d runs", n, before, got)
					Expect(w.queuedRuns()).To(Equal(1), "a task has exactly one waiting run after a message")
				case op < 7 && w.cur == nil:
					w.claim()
				case op < 9 && w.cur != nil:
					w.prompt()
				case op < 10 && w.cur != nil:
					w.restart()
				case op < 11 && w.cur != nil && w.cur.PromptThroughSeq.Valid:
					w.finish()
				}
			}
			// Drain: prompt and end the run in flight, then run until nothing is queued.
			for w.cur != nil || w.claim() {
				w.prompt()
				w.finish()
			}

			Expect(w.delivered).To(HaveLen(w.sent), "a message was never in a prompt")
			for text, n := range w.delivered {
				Expect(n).To(Equal(1), "message %s was in %d prompts", text, n)
			}
			Expect(w.runs).To(HaveLen(w.claims), "every run created was claimed once")
		})
	})
})
