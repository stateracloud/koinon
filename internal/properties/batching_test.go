// SPDX-License-Identifier: Apache-2.0

package properties_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"

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
	ctx         context.Context
	org, user   pgtype.UUID
	task        store.Task
	cur         *store.Run      // the run an agentd owns now, if any
	sent        int             // messages appended
	delivered   map[string]int  // message text -> times it was put in a prompt
	runs        map[string]bool // run IDs that ever existed
	promptSizes []int
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

// send appends one user message and enqueues a run, in one transaction (the lock on the task row
// serialises concurrent senders). It reports whether a run was queued.
func (w *taskWorld) send() bool {
	GinkgoHelper()
	w.sent++
	text := fmt.Sprintf("m%d", w.sent)
	payload, _ := json.Marshal(map[string]string{"text": text})
	queued := false
	Expect(pgx.BeginFunc(w.ctx, pool, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if _, err := tq.AppendTaskEvent(w.ctx, store.AppendTaskEventParams{
			OrgID: w.org, TaskID: w.task.ID, Kind: "message", ActorKind: store.TaskActorKindUser, ActorUserID: w.user, Payload: payload,
		}); err != nil {
			return err
		}
		run, ok, err := scheduler.Enqueue(w.ctx, tq, w.task)
		queued = ok
		if ok {
			w.runs[uuidString(run.ID)] = true
		}
		return err
	})).To(Succeed())
	return queued
}

func uuidString(u pgtype.UUID) string { return fmt.Sprintf("%x", u.Bytes) }

// claim is an agentd claiming the next run, marking it running and building its prompt exactly as
// agentd.prompt does: from what the task's other runs did not send, or from where this run's own
// prompt ended when it was resumed.
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
	w.prompt()
	return true
}

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
	through := after
	for _, m := range msgs {
		var b struct{ Text string }
		Expect(json.Unmarshal(m.Payload, &b)).To(Succeed())
		w.delivered[b.Text]++
		through = m.Seq
	}
	w.promptSizes = append(w.promptSizes, len(msgs))
	updated, err := q.SetRunPromptThrough(w.ctx, store.SetRunPromptThroughParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch, PromptThroughSeq: through})
	Expect(err).NotTo(HaveOccurred())
	w.cur = &updated
}

// restart kills the owner: another agentd takes the run over under a new epoch and, if it is
// not yet prompted, prompts it; a prompted run is resumed after its own prompt.
func (w *taskWorld) restart() {
	GinkgoHelper()
	run := *w.cur
	_, err := q.ReleaseRunLease(w.ctx, store.ReleaseRunLeaseParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	taken, err := q.TakeOverExpiredRun(w.ctx, store.TakeOverExpiredRunParams{OrgID: w.org, OwnerID: "b", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	Expect(taken.ID).To(Equal(run.ID))
	Expect(taken.Epoch).To(BeNumerically(">", run.Epoch))
	w.cur = &taken
	w.prompt()
}

func (w *taskWorld) finish() {
	GinkgoHelper()
	run := *w.cur
	_, err := q.FinishRun(w.ctx, store.FinishRunParams{OrgID: w.org, ID: run.ID, Epoch: run.Epoch, Outcome: store.RunStateSucceeded})
	Expect(err).NotTo(HaveOccurred())
	w.cur = nil
}

var _ = Describe("batching (issue 26)", func() {
	// Property: for any interleaving of messages, runs starting, owner restarts and runs ending,
	// no message is lost or put in a prompt twice, at most one run waits in the queue, and a burst
	// of messages with no run claimed between them queues exactly one run.
	It("never loses or duplicates an event", func(ctx SpecContext) {
		propseed.ForEach(func(rng *rand.Rand) {
			w := newTaskWorld(ctx)
			burst, burstQueued := 0, 0
			for step := 0; step < 30+rng.Intn(30); step++ {
				switch op := rng.Intn(10); {
				case op < 5: // send, often in bursts
					for n := 1 + rng.Intn(4); n > 0; n-- {
						if w.send() {
							burstQueued++
						}
						burst++
					}
					Expect(w.queuedRuns()).To(BeNumerically("<=", 1), "two runs queued for one task")
				case op < 7 && w.cur == nil:
					if w.claim() {
						// The claim ends the burst: it queued one run for all its messages.
						Expect(burstQueued).To(BeNumerically("<=", 1), "a burst of %d messages queued %d runs", burst, burstQueued)
						burst, burstQueued = 0, 0
					}
				case op < 8 && w.cur != nil:
					w.restart()
				case op < 9 && w.cur != nil:
					w.finish()
				}
			}
			// Drain: end the run in flight, then run until nothing is queued.
			if w.cur != nil {
				w.finish()
			}
			for w.claim() {
				w.finish()
			}

			Expect(w.delivered).To(HaveLen(w.sent), "a message was never in a prompt")
			for text, n := range w.delivered {
				Expect(n).To(Equal(1), "message %s was in %d prompts", text, n)
			}
			Expect(len(w.runs)).To(BeNumerically("<=", w.sent), "more runs than messages")
		})
	})
})
