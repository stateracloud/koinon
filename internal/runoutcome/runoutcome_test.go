// SPDX-License-Identifier: Apache-2.0

package runoutcome_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// index is the position of the first occurrence of v, or -1.
func index(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

var _ = Describe("Record", func() {
	It("numbers the events under one source seq and writes a repeated seq once", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.Record(ctx, "run.one", map[string]int{"n": 1})).To(Succeed())
		Expect(w.own.Record(ctx, "run.two", map[string]int{"n": 2})).To(Succeed())
		Expect(w.runKinds(ctx)).To(Equal([]string{"run.one", "run.two"}))
		// A second Owner of the same epoch starts at seq 1 again: its first event is the retry of an
		// event that committed, and is not written twice.
		again := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: w.run})
		Expect(again.Record(ctx, "run.one", map[string]int{"n": 1})).To(Succeed())
		Expect(w.runKinds(ctx)).To(Equal([]string{"run.one", "run.two"}))
	})

	It("writes nothing for a stale epoch and fences as lost", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, Outcome: store.RunStateSucceeded})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.own.Record(ctx, "run.late", nil)).To(MatchError(runoutcome.ErrFenced))
		Expect(w.own.IsFenced()).To(BeTrue())
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceLost))
		Expect(w.runKinds(ctx)).To(BeEmpty())
		// Once fenced, it does not even try.
		Expect(w.own.Record(ctx, "run.later", nil)).To(MatchError(runoutcome.ErrFenced))
	})

	It("tells a cancel from a lost epoch", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "a person canceled it"})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.own.Record(ctx, "run.late", nil)).To(MatchError(runoutcome.ErrFenced))
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
	})
})

var _ = Describe("the source seq under concurrent writers", func() {
	It("only grows under the write mutex: Record and an ending verb never share or skip a seq", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		turn.Reason = "end_turn"
		const writers = 24
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				defer GinkgoRecover()
				// Records that run after the ending are fenced; the ones before it are written.
				err := w.own.Record(ctx, "run.concurrent", map[string]int{"n": i})
				if err != nil {
					Expect(err).To(MatchError(runoutcome.ErrFenced))
				}
			})
		}
		wg.Go(func() {
			defer GinkgoRecover()
			Expect(w.own.Succeed(ctx, turn)).To(Succeed())
		})
		wg.Wait()

		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: w.run.ID, MaxEvents: 1000})
		Expect(err).NotTo(HaveOccurred())
		Expect(evs).NotTo(BeEmpty())
		var src []int64
		for _, e := range evs {
			Expect(e.SourceSeq.Valid).To(BeTrue())
			src = append(src, e.SourceSeq.Int64)
		}
		// In the order they were written the seqs are 1, 2, 3, ...: unique and contiguous.
		want := make([]int64, len(evs))
		for i := range want {
			want[i] = int64(i + 1)
		}
		Expect(src).To(Equal(want))
		// The ending is the last event of the run.
		Expect(evs[len(evs)-1].Kind).To(Equal("run.succeeded"))
	})
})

var _ = Describe("EndTurn", func() {
	It("succeeds a run with nothing to wait for: turn row, run state, reply, then the task", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		w.says(ctx, w.own, "All done.")

		end, err := w.own.EndTurn(ctx, runoutcome.Delivered{}, turn)
		Expect(err).NotTo(HaveOccurred())
		Expect(end.Parked).To(BeFalse())

		Expect(w.turnRow(ctx).EndReason.String).To(Equal("end_turn"))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue())
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateSucceeded))
		Expect(w.runKinds(ctx)).To(Equal([]string{"acp.agent_message_chunk", "run.succeeded"}))
		// The reply comes before the task's end on its timeline.
		kinds := w.taskKinds(ctx)
		Expect(index(kinds, "message")).To(BeNumerically(">=", 0))
		Expect(index(kinds, "message")).To(BeNumerically("<", index(kinds, "status.changed")))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateCompleted))
		Expect(w.metrics.finished).To(Equal([]store.RunState{store.RunStateSucceeded}))
	})

	It("parks on an open approval: turn row, reply, park note, run state, task state", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		w.says(ctx, w.own, "I asked for approval.")
		req := w.pendingApproval(ctx)

		end, err := w.own.EndTurn(ctx, runoutcome.Delivered{}, turn)
		Expect(err).NotTo(HaveOccurred())
		Expect(end.Parked).To(BeTrue())
		Expect(end.Waiting).To(BeTrue())
		Expect(end.ParkReason).To(Equal(store.RunParkReasonApproval))

		Expect(w.turnRow(ctx).EndReason.String).To(Equal("park:approval"))
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateParked))
		Expect(run.ParkReason.RunParkReason).To(Equal(store.RunParkReasonApproval))
		Expect(w.runKinds(ctx)).To(Equal([]string{"acp.agent_message_chunk", runoutcome.KindParked}))
		// The reply is on the task before the park note.
		kinds := w.taskKinds(ctx)
		Expect(index(kinds, "message")).To(BeNumerically(">=", 0))
		Expect(index(kinds, "message")).To(BeNumerically("<", index(kinds, runoutcome.KindParked)))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateInputRequired))
		var note struct{ Approvals []string }
		for _, e := range w.taskEvents(ctx) {
			if e.Kind == runoutcome.KindParked {
				Expect(json.Unmarshal(e.Payload, &note)).To(Succeed())
			}
		}
		Expect(note.Approvals).To(ConsistOf(req.ID.String()))
		Expect(w.metrics.finished).To(Equal([]store.RunState{store.RunStateParked}))
	})

	It("marks the outcomes the prompt carried as delivered and then succeeds", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		req := w.pendingApproval(ctx)
		_, err := pool.Exec(ctx, `UPDATE approval_requests SET status = 'executed', decided_by = $3 WHERE org_id = $1 AND id = $2`, w.org, req.ID, w.user)
		Expect(err).NotTo(HaveOccurred())

		end, err := w.own.EndTurn(ctx, runoutcome.Delivered{Approvals: []pgtype.UUID{req.ID}}, runoutcome.Turn{})
		Expect(err).NotTo(HaveOccurred())
		Expect(end.Parked).To(BeFalse())
		undelivered, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: w.org, RunID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(undelivered).To(BeEmpty())
		Expect(w.current(ctx).State).To(Equal(store.RunStateSucceeded))
	})

	It("succeeds without a park when the agent canceled the task in the turn", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		w.pendingApproval(ctx)
		_, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: w.org, ID: w.task, State: store.TaskStateCanceled})
		Expect(err).NotTo(HaveOccurred())

		end, err := w.own.EndTurn(ctx, runoutcome.Delivered{}, turn)
		Expect(err).NotTo(HaveOccurred())
		Expect(end.Parked).To(BeFalse())
		Expect(w.current(ctx).State).To(Equal(store.RunStateSucceeded))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateCanceled))
		Expect(w.taskKinds(ctx)).NotTo(ContainElement("status.changed"))
		Expect(w.turnRow(ctx).EndReason.String).To(Equal("end_turn"))
	})

	It("writes nothing for a stale epoch: the turn row stays open and the task is untouched", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())

		_, err = w.own.EndTurn(ctx, runoutcome.Delivered{}, turn)
		Expect(err).To(MatchError(runoutcome.ErrFenced))
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeFalse())
		Expect(w.taskEvents(ctx)).To(BeEmpty())
		Expect(w.metrics.finished).To(BeEmpty())
	})

	It("orders the decision against the approval service's resolution of the park", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		req := w.pendingApproval(ctx)

		// The approval service holds the run's row lock while it resolves the request.
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = store.New(tx).LockRun(ctx, store.LockRunParams{OrgID: w.org, ID: w.run.ID})
		Expect(err).NotTo(HaveOccurred())

		type result struct {
			end runoutcome.TurnEnd
			err error
		}
		done := make(chan result, 1)
		go func() {
			defer GinkgoRecover()
			end, err := w.own.EndTurn(ctx, runoutcome.Delivered{}, turn)
			done <- result{end, err}
		}()
		Consistently(done, 300*time.Millisecond).ShouldNot(Receive(), "the turn's end waits for the lock")

		_, err = tx.Exec(ctx, `UPDATE approval_requests SET status = 'executed', decided_by = $3 WHERE org_id = $1 AND id = $2`, w.org, req.ID, w.user)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Commit(ctx)).To(Succeed())

		var res result
		Eventually(done).Should(Receive(&res))
		Expect(res.err).NotTo(HaveOccurred())
		// It saw the decision: the run parks, nobody is waited for, and the park is resolved at once.
		Expect(res.end.Parked).To(BeTrue())
		Expect(res.end.Waiting).To(BeFalse())
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateParked))
		Expect(run.ReadyAt.Valid).To(BeTrue(), "ResolveRunPark made the run claimable")
		Expect(w.taskState(ctx)).NotTo(Equal(store.TaskStateInputRequired))
	})
})

var _ = Describe("Succeed", func() {
	It("ends the turn row, then the run, then completes the task", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		turn.Reason = "end_turn"
		Expect(w.own.Succeed(ctx, turn)).To(Succeed())
		Expect(w.turnRow(ctx).EndReason.String).To(Equal("end_turn"))
		Expect(w.current(ctx).State).To(Equal(store.RunStateSucceeded))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateCompleted))
		// The session restart matches this event literally (RequestSessionRestartForAgent, #81).
		var last store.TaskEvent
		for _, ev := range w.taskEvents(ctx) {
			if ev.Kind == "status.changed" {
				last = ev
			}
		}
		Expect(last.ActorKind).To(Equal(store.TaskActorKindSystem))
		var payload struct{ To, Reason string }
		Expect(json.Unmarshal(last.Payload, &payload)).To(Succeed())
		Expect(payload.To).To(Equal(string(store.TaskStateCompleted)))
		Expect(payload.Reason).To(Equal(runoutcome.ReasonRunSucceeded))
		Expect(payload.Reason).To(Equal("run_succeeded"), "the literal in queries/stuck.sql")
	})
})

var _ = Describe("Supersede", func() {
	It("ends the turn row, then the run, then completes the task, from starting", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		turn := w.openTurn(ctx)
		Expect(w.own.Supersede(ctx, turn, "read already")).To(Succeed())
		Expect(w.turnRow(ctx).EndReason.String).To(Equal("superseded"))
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateSucceeded))
		Expect(run.OutcomeReason.String).To(Equal("read already"))
		Expect(w.runKinds(ctx)).To(Equal([]string{"run.succeeded"}))
		Expect(w.taskKinds(ctx)).NotTo(ContainElement("message"))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateCompleted))
		Expect(w.metrics.finished).To(Equal([]store.RunState{store.RunStateSucceeded}))
	})

	It("writes nothing for a stale epoch, the turn row included", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		turn := w.openTurn(ctx)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.own.Supersede(ctx, turn, "late")).To(MatchError(runoutcome.ErrFenced))
		Expect(w.current(ctx).State).To(Equal(store.RunStateCanceled))
		Expect(w.runKinds(ctx)).To(BeEmpty())
		Expect(w.metrics.finished).To(BeEmpty())
	})
})

var _ = Describe("Fail", func() {
	It("fails the task of a fatal failure after the run's failure on its timeline", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		Expect(w.own.Fail(ctx, turn, "the harness crashed", store.RunFailureClassFatal)).To(Succeed())

		Expect(w.turnRow(ctx).EndReason.String).To(Equal("failed"))
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateFailed))
		Expect(run.OutcomeReason.String).To(Equal("the harness crashed"))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
		Expect(w.runKinds(ctx)).To(Equal([]string{"run.failed"}))
		kinds := w.taskKinds(ctx)
		Expect(index(kinds, "run.failed")).To(BeNumerically("<", index(kinds, "status.changed")))
		Expect(w.taskState(ctx)).To(Equal(store.TaskStateFailed))
		Expect(w.metrics.finished).To(Equal([]store.RunState{store.RunStateFailed}))
	})

	It("leaves the task open for a failure a person can retry", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.Fail(ctx, runoutcome.Turn{}, "the provider is throttling", store.RunFailureClassThrottled)).To(Succeed())
		Expect(w.current(ctx).State).To(Equal(store.RunStateFailed))
		Expect(w.taskKinds(ctx)).To(Equal([]string{"run.failed"}))
		Expect(w.taskState(ctx)).NotTo(Equal(store.TaskStateFailed))
	})

	It("fails a looping run with class loop and leaves its task open for a person to answer", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		Expect(w.own.Fail(ctx, turn, "the agent kept looping after it was asked to change its approach", store.RunFailureClassLoop)).To(Succeed())
		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateFailed))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassLoop))
		Expect(w.taskKinds(ctx)).To(Equal([]string{"run.failed"}))
		Expect(w.taskState(ctx)).NotTo(Equal(store.TaskStateFailed))
	})

	It("writes nothing for a stale epoch, the turn row included", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: w.org, ID: w.run.ID, Epoch: w.run.Epoch, Outcome: store.RunStateSucceeded})
		Expect(err).NotTo(HaveOccurred())

		Expect(w.own.Fail(ctx, turn, "late", store.RunFailureClassFatal)).To(MatchError(runoutcome.ErrFenced))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeFalse())
		Expect(w.runKinds(ctx)).To(BeEmpty())
		Expect(w.taskEvents(ctx)).To(BeEmpty())
		Expect(w.metrics.finished).To(BeEmpty())
	})
})

var _ = Describe("a verb that loses the race with a cancel", func() {
	It("still ends the turn row when Fail or Requeue is fenced as canceled", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())

		Expect(w.own.Fail(ctx, turn, "late", store.RunFailureClassFatal)).To(MatchError(runoutcome.ErrFenced))
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue())
		Expect(w.turnRow(ctx).EndReason.String).To(Equal("failed"))
	})

	It("ends the turn row of a requeue fenced by a cancel", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		turn := w.openTurn(ctx)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())

		Expect(w.own.Requeue(ctx, turn, "late")).To(MatchError(runoutcome.ErrFenced))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue())
	})
})

var _ = Describe("Cancel", func() {
	It("cancels with the failure class and the extra fields, which do not override the class", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		Expect(w.own.Cancel(ctx, turn, "wall clock reached", store.RunFailureClassBudget,
			map[string]any{"note": "work in progress", "failureClass": "ignored"})).To(Succeed())

		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue())
		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: w.run.ID, MaxEvents: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(evs).To(HaveLen(1))
		Expect(evs[0].Kind).To(Equal("run.canceled"))
		var body map[string]any
		Expect(json.Unmarshal(evs[0].Payload, &body)).To(Succeed())
		Expect(body).To(HaveKeyWithValue("note", "work in progress"))
		Expect(body).To(HaveKeyWithValue("reason", "wall clock reached"))
		// The class wins over the extra field of the same name.
		Expect(body).To(HaveKeyWithValue("failureClass", "budget"))
		// A canceled run leaves the task as it is.
		Expect(w.taskState(ctx)).NotTo(Equal(store.TaskStateCompleted))
		Expect(w.taskKinds(ctx)).To(Equal([]string{"run.canceled"}))
	})
})

var _ = Describe("Requeue", func() {
	It("puts a starting run back in the queue and counts it", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		Expect(w.own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")).To(Succeed())
		Expect(w.current(ctx).State).To(Equal(store.RunStateQueued))
		Expect(w.runKinds(ctx)).To(Equal([]string{"run.requeued"}))
		Expect(w.metrics.requeued).To(Equal(1))
	})

	It("is fenced when the run was taken over or ended meanwhile", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.own.Requeue(ctx, runoutcome.Turn{}, "late")).To(MatchError(runoutcome.ErrFenced))
		Expect(w.current(ctx).State).To(Equal(store.RunStateCanceled))
		Expect(w.metrics.requeued).To(BeZero())
	})
})

var _ = Describe("Release", func() {
	It("ends the turn row and gives the lease up for a takeover", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		turn.Reason = "interrupted"
		w.own.Release(ctx, turn)
		Expect(w.turnRow(ctx).EndReason.String).To(Equal("interrupted"))
		Expect(w.current(ctx).LeaseExpiresAt.Time).To(BeTemporally("<=", time.Now()))
	})

	It("does not touch a run that another epoch owns", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())
		w.own.Release(ctx, runoutcome.Turn{})
		Expect(w.current(ctx).State).To(Equal(store.RunStateCanceled))
	})
})

var _ = Describe("SandboxUnavailable", func() {
	It("fails the run, marks the agent unhealthy, tells the admins and notes the task", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		turn := w.openTurn(ctx)
		Expect(w.own.SandboxUnavailable(ctx, turn, runoutcome.Unavailable{
			Reason: "the sandbox never became ready", Cause: execution.WaitCauseImagePull, Detail: "pull backoff",
			Waited: 11 * time.Minute, Cap: 10 * time.Minute, Class: store.RunFailureClassSandboxUnavailable,
		})).To(Succeed())

		run := w.current(ctx)
		Expect(run.State).To(Equal(store.RunStateFailed))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassSandboxUnavailable))
		Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue())
		agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: w.org, ID: w.agent})
		Expect(err).NotTo(HaveOccurred())
		Expect(agent.UnhealthyAt.Valid).To(BeTrue())
		notes, err := q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: w.org, UserID: w.user, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(notes).To(HaveLen(1))
		Expect(notes[0].Kind).To(Equal(runoutcome.NotificationAgentUnhealthy))
		kinds := w.taskKinds(ctx)
		Expect(index(kinds, "run.failed")).To(BeNumerically("<", index(kinds, runoutcome.KindAgentUnhealthy)))
		Expect(w.metrics.finished).To(Equal([]store.RunState{store.RunStateFailed}))
		Expect(w.metrics.capped).To(Equal([]string{"image_pull"}))
	})

	It("writes none of it for a stale epoch", func(ctx SpecContext) {
		w := newWorld(ctx, false)
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())
		err = w.own.SandboxUnavailable(ctx, runoutcome.Turn{}, runoutcome.Unavailable{
			Reason: "late", Cause: execution.WaitCauseImagePull, Class: store.RunFailureClassSandboxUnavailable,
		})
		Expect(err).To(MatchError(runoutcome.ErrFenced))
		agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: w.org, ID: w.agent})
		Expect(err).NotTo(HaveOccurred())
		Expect(agent.UnhealthyAt.Valid).To(BeFalse())
		Expect(w.taskEvents(ctx)).To(BeEmpty())
	})
})

var _ = Describe("a worker that is shutting down", func() {
	It("returns the pool's reason for a failed write, not a fence", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		errShutdown := errors.New("shutting down")
		own := runoutcome.New(runoutcome.Config{
			DB: pool, Queries: q, Run: w.run,
			Stop: func(context.Context) error { return errShutdown },
		})
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "canceled"})
		Expect(err).NotTo(HaveOccurred())
		Expect(own.Fail(ctx, runoutcome.Turn{}, "late", store.RunFailureClassFatal)).To(MatchError(errShutdown))
		Expect(own.IsFenced()).To(BeFalse())
	})
})

var _ = Describe("RecordUpdate: items, not chunks (old issue 344)", func() {
	chunk := func(kind, text string) []byte {
		return []byte(`{"sessionUpdate":"` + kind + `","content":{"type":"text","text":"` + text + `"}}`)
	}
	texts := func(ctx context.Context, w *world) (kinds, text []string, partial []bool) {
		GinkgoHelper()
		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: w.run.ID, MaxEvents: 1000})
		Expect(err).NotTo(HaveOccurred())
		for _, e := range evs {
			var u struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
				Partial bool `json:"partial"`
			}
			Expect(json.Unmarshal(e.Payload, &u)).To(Succeed())
			kinds, text, partial = append(kinds, e.Kind), append(text, u.Content.Text), append(partial, u.Partial)
		}
		return
	}

	It("flushes the live chunks before it stores the item they make up", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		var stored []int
		own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: w.run, LiveFlush: func() {
			stored = append(stored, len(w.runKinds(ctx)))
		}})
		Expect(own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "ab"))).To(Succeed())
		Expect(own.RecordUpdate(ctx, "acp.tool_call", []byte(`{"sessionUpdate":"tool_call","toolCallId":"t1"}`))).To(Succeed())
		Expect(stored).To(Equal([]int{0}), "flushed once, with the item not yet stored")
	})

	It("stores one event per message, however many chunks it came in, and sends every chunk live", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		var live []string
		own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: w.run, Live: func(kind, item string, p []byte) { live = append(live, kind) }})
		for range 500 {
			Expect(own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "ab"))).To(Succeed())
		}
		Expect(w.runKinds(ctx)).To(BeEmpty(), "an item in progress is not stored")
		Expect(own.RecordUpdate(ctx, "acp.tool_call", []byte(`{"sessionUpdate":"tool_call","toolCallId":"t1"}`))).To(Succeed())
		Expect(own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "done"))).To(Succeed())
		own.FlushItems(ctx, false)

		kinds, text, partial := texts(ctx, w)
		Expect(kinds).To(Equal([]string{"acp.agent_message_chunk", "acp.tool_call", "acp.agent_message_chunk"}))
		Expect(text[0]).To(Equal(strings.Repeat("ab", 500)))
		Expect(text[2]).To(Equal("done"))
		Expect(partial).To(Equal([]bool{false, false, false}))
		Expect(live).To(HaveLen(501))
	})

	It("gives the stored event and its live chunks the same item name, and each item its own", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		liveItems := map[string]int{}
		var order []string
		own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: w.run, Live: func(kind, item string, p []byte) {
			if liveItems[item] == 0 {
				order = append(order, item)
			}
			liveItems[item]++
		}})
		for _, c := range []struct{ kind, text string }{
			{"agent_thought_chunk", "a"}, {"agent_thought_chunk", "b"}, {"agent_message_chunk", "c"}, {"agent_message_chunk", "d"}, {"agent_message_chunk", "e"},
		} {
			Expect(own.RecordUpdate(ctx, "acp."+c.kind, chunk(c.kind, c.text))).To(Succeed())
		}
		own.FlushItems(ctx, false)

		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: w.run.ID, MaxEvents: 1000})
		Expect(err).NotTo(HaveOccurred())
		var stored []string
		for _, e := range evs {
			var u struct {
				Item string `json:"item"`
			}
			Expect(json.Unmarshal(e.Payload, &u)).To(Succeed())
			stored = append(stored, u.Item)
		}
		Expect(stored).To(HaveLen(2))
		Expect(stored[0]).NotTo(Equal(stored[1]))
		Expect(order).To(Equal(stored), "the live chunks carry the names of the stored events")
		Expect(liveItems[stored[0]]).To(Equal(2))
		Expect(liveItems[stored[1]]).To(Equal(3))
	})

	It("keeps a thought and a message apart", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		for _, c := range []struct{ kind, text string }{
			{"agent_thought_chunk", "hm"}, {"agent_thought_chunk", "m"}, {"agent_message_chunk", "ok"},
		} {
			Expect(w.own.RecordUpdate(ctx, "acp."+c.kind, chunk(c.kind, c.text))).To(Succeed())
		}
		w.own.FlushItems(ctx, false)
		kinds, text, _ := texts(ctx, w)
		Expect(kinds).To(Equal([]string{"acp.agent_thought_chunk", "acp.agent_message_chunk"}))
		Expect(text).To(Equal([]string{"hmm", "ok"}))
	})

	It("stores a message cut off mid-way with a partial marker", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "half a "))).To(Succeed())
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "sentence"))).To(Succeed())
		w.own.FlushItems(ctx, true)
		w.own.FlushItems(ctx, true) // nothing left: stores nothing more
		kinds, text, partial := texts(ctx, w)
		Expect(kinds).To(Equal([]string{"acp.agent_message_chunk"}))
		Expect(text).To(Equal([]string{"half a sentence"}))
		Expect(partial).To(Equal([]bool{true}))
	})

	It("stores the item in progress, cut off, after the control plane canceled the run, and nothing else", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "said so far"))).To(Succeed())
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		w.own.FenceFromDB(ctx)
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))

		w.own.FlushItems(ctx, true)
		Expect(w.own.RecordRaw(ctx, "run.prompt", []byte(`{}`))).To(MatchError(runoutcome.ErrFenced))
		kinds, text, partial := texts(ctx, w)
		Expect(kinds).To(ContainElement("acp.agent_message_chunk"))
		Expect(text).To(ContainElement("said so far"))
		Expect(partial).To(ContainElement(true))
		Expect(kinds).NotTo(ContainElement("run.prompt"))
	})

	It("refuses an event that races the cancel: the fence is set after RecordRaw's check (old issue 660)", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "said so far"))).To(Succeed())
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: w.org, ID: w.run.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		// The Owner has not noticed the cancel yet: RecordRaw's IsFenced check passes, and the
		// pending item is flushed as a normal event, which the canceled run refuses.
		Expect(w.own.IsFenced()).To(BeFalse())
		Expect(w.own.RecordRaw(ctx, "acp.tool_call", []byte(`{}`))).To(MatchError(runoutcome.ErrFenced))
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
		kinds, _, _ := texts(ctx, w)
		Expect(kinds).NotTo(ContainElement("acp.tool_call"))
		Expect(kinds).NotTo(ContainElement("acp.agent_message_chunk"))
	})

	It("stores the item in progress, cut off, before a verb ends the run, so the reply has it", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		turn := w.openTurn(ctx)
		turn.Reason = "canceled"
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", "so far"))).To(Succeed())
		Expect(w.own.Succeed(ctx, turn)).To(Succeed())
		kinds, text, partial := texts(ctx, w)
		Expect(kinds).To(Equal([]string{"acp.agent_message_chunk", "run.succeeded"}))
		Expect(text[0]).To(Equal("so far"))
		Expect(partial[0]).To(BeTrue())
	})

	It("stores a very long message in pieces that join to the whole", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		piece := strings.Repeat("x", 1000)
		for range 150 {
			Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", chunk("agent_message_chunk", piece))).To(Succeed())
		}
		w.own.FlushItems(ctx, false)
		kinds, text, _ := texts(ctx, w)
		Expect(kinds).To(HaveLen(3), "150 KB in items of at most 64 KiB")
		Expect(strings.Join(text, "")).To(Equal(strings.Repeat(piece, 150)))
	})

	It("stores a chunk that is not plain text as it is", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		img := []byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"AAAA","mimeType":"image/png"}}`)
		Expect(w.own.RecordUpdate(ctx, "acp.agent_message_chunk", img)).To(Succeed())
		Expect(w.runKinds(ctx)).To(Equal([]string{"acp.agent_message_chunk"}))
	})
})
