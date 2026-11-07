// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/runwatch"
)

func countRunEvents(ctx context.Context, e *env, run store.Run) int {
	var n int
	Expect(e.pool.QueryRow(ctx, "SELECT count(*) FROM run_events WHERE org_id = $1 AND run_id = $2", run.OrgID, run.ID).Scan(&n)).To(Succeed())
	return n
}

// watcher reads one WatchRun stream in the background.
type watcher struct {
	cancel context.CancelFunc
	items  chan *runv1alpha1.WatchRunResponse
	done   chan error
}

func watch(ctx context.Context, c *client, org, run string, after int64) *watcher {
	ctx, cancel := context.WithCancel(ctx)
	w := &watcher{cancel: cancel, items: make(chan *runv1alpha1.WatchRunResponse, 1024), done: make(chan error, 1)}
	stream, err := c.runs.WatchRun(ctx, &runv1alpha1.WatchRunRequest{OrgId: org, RunId: run, AfterSeq: after})
	if err != nil {
		cancel()
		w.done <- err
		close(w.items)
		return w
	}
	go func() {
		defer close(w.items)
		defer func() { _ = stream.Close() }()
		for {
			msg, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				w.done <- nil
				return
			}
			if err != nil {
				w.done <- err
				return
			}
			w.items <- msg
		}
	}()
	DeferCleanup(cancel)
	return w
}

// nextEvent waits for the next event, skipping run snapshots.
func (w *watcher) nextEvent(timeout time.Duration) *runv1alpha1.RunEvent {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-w.items:
			if !ok {
				Fail("stream ended before the next event")
			}
			if ev := msg.GetEvent(); ev != nil {
				return ev
			}
		case <-deadline:
			Fail("no event within " + timeout.String())
		}
	}
}

var _ = Describe("WatchRun", func() {
	var (
		e   *env
		f   orgFixture
		ad  *agentd
		run store.Run
	)
	BeforeEach(func(ctx SpecContext) {
		// Polling is effectively off: live events must come through NOTIFY.
		e = newEnv(ctx, envOptions{watchPoll: time.Hour})
		f = e.newOrgFixture(ctx)
		ad = e.agentd("agentd-a")
		run = ad.mustClaim(ctx, f.org)
		Expect(run.ID.String()).To(Equal(f.run.GetId()))
	})

	It("replays stored events after the given seq, then streams live ones", func(ctx SpecContext) {
		// seq 1 is the scheduler's run.queued event.
		for i := 2; i <= 4; i++ {
			Expect(ad.event(ctx, run, i).Seq).To(Equal(int64(i)))
		}
		w := watch(ctx, f.owner, f.org, f.run.GetId(), 2)
		first := <-w.items
		Expect(first.GetRun().GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_RUNNING))
		Expect(w.nextEvent(5 * time.Second).GetSeq()).To(Equal(int64(3)))
		Expect(w.nextEvent(5 * time.Second).GetSeq()).To(Equal(int64(4)))

		ad.event(ctx, run, 5)
		ev := w.nextEvent(time.Second)
		Expect(ev.GetSeq()).To(Equal(int64(5)))
		Expect(ev.GetKind()).To(Equal("session.update"))
		Expect(ev.GetPayload().AsMap()).To(HaveKeyWithValue("n", BeEquivalentTo(5)))
	})

	It("streams the text the agent is typing, live and unstored (old issue 344)", func(ctx SpecContext) {
		w := watch(ctx, f.owner, f.org, f.run.GetId(), 0)
		Expect((<-w.items).GetRun()).NotTo(BeNil())
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))
		before := countRunEvents(ctx, e, run)

		// agentd's publisher batches chunks into NOTIFYs; polling is off, so only NOTIFY can deliver them.
		pub := runwatch.NewChunkPublisher(e.pool, run.OrgID, run.ID, 20*time.Millisecond, nil)
		for _, t := range []string{"Hel", "lo ", "wor", "ld"} {
			pub.Add("acp.agent_message_chunk", "1-1", []byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+t+`"}}`))
		}
		pub.Add("acp.agent_thought_chunk", "1-2", []byte(`{"content":{"type":"text","text":"hm"}}`))
		pub.Close()

		var text, thought string
		deadline := time.After(5 * time.Second)
		for text != "Hello world" || thought != "hm" {
			select {
			case msg, ok := <-w.items:
				Expect(ok).To(BeTrue(), "stream ended before the live text arrived")
				Expect(msg.GetEvent()).To(BeNil(), "chunks are not events")
				for _, c := range msg.GetLive().GetChunks() {
					if c.GetKind() == "acp.agent_message_chunk" {
						text += c.GetText()
					} else {
						thought += c.GetText()
					}
				}
			case <-deadline:
				Fail("live text not received: " + text + " / " + thought)
			}
		}
		Expect(countRunEvents(ctx, e, run)).To(Equal(before), "chunks are never stored")
	})

	It("names the item on every live batch, and the event that replaces it names the same item (old issue 344)", func(ctx SpecContext) {
		w := watch(ctx, f.owner, f.org, f.run.GetId(), 0)
		Expect((<-w.items).GetRun()).NotTo(BeNil())
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))

		// A tick far away: only Flush sends, as the Owner does before it stores the item.
		pub := runwatch.NewChunkPublisher(e.pool, run.OrgID, run.ID, time.Hour, nil)
		defer pub.Close()
		pub.Add("acp.agent_message_chunk", "1-1", []byte(`{"content":{"type":"text","text":"Hello world"}}`))
		pub.Flush()
		ad.item(ctx, run, "1-1")

		var liveItems, eventItems []string
		for len(liveItems) < 1 || len(eventItems) < 1 {
			select {
			case msg, ok := <-w.items:
				Expect(ok).To(BeTrue())
				for _, c := range msg.GetLive().GetChunks() {
					liveItems = append(liveItems, c.GetItem())
				}
				if ev := msg.GetEvent(); ev != nil {
					eventItems = append(eventItems, ev.GetPayload().AsMap()["item"].(string))
				}
			case <-time.After(5 * time.Second):
				Fail("nothing arrived")
			}
		}
		Expect(liveItems).To(Equal([]string{"1-1"}))
		Expect(eventItems).To(Equal([]string{"1-1"}))
	})

	It("lets a client drop every live tail of a stored item, whatever order they arrive in, even when the stream is busy (old issue 344)", func(ctx SpecContext) {
		w := watch(ctx, f.owner, f.org, f.run.GetId(), 0)
		Expect((<-w.items).GetRun()).NotTo(BeNil())
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))

		// The batch and the event's wake-up are both queued for the stream, again and again. The
		// server sends a batch before its event when it can, but the client must not depend on it.
		pub := runwatch.NewChunkPublisher(e.pool, run.OrgID, run.ID, time.Hour, nil)
		defer pub.Close()
		const rounds = 30
		for i := 0; i < rounds; i++ {
			name := fmt.Sprintf("1-%d", i+1)
			pub.Add("acp.agent_message_chunk", name, []byte(`{"content":{"type":"text","text":"tail"}}`))
			pub.Flush()
			ad.item(ctx, run, name)
		}
		// Per round, the live batch and the stored event share the item name, whatever the order.
		live := map[string]bool{}
		stored := map[string]bool{}
		for len(stored) < rounds {
			select {
			case msg, ok := <-w.items:
				Expect(ok).To(BeTrue())
				for _, c := range msg.GetLive().GetChunks() {
					Expect(c.GetItem()).NotTo(BeEmpty(), "every live chunk names its item")
					live[c.GetItem()] = true
				}
				if ev := msg.GetEvent(); ev != nil {
					stored[ev.GetPayload().AsMap()["item"].(string)] = true
				}
			case <-time.After(5 * time.Second):
				Fail("nothing arrived")
			}
		}
		for name := range live {
			Expect(stored).To(HaveKey(name), "a live batch names an item that is stored")
		}
	})

	It("sends the item a canceled run's worker stores after the cancel, then a second final snapshot (old issue 344)", func(ctx SpecContext) {
		quick := newEnvOn(ctx, e, envOptions{watchPoll: time.Hour, watchCancelGrace: 3 * time.Second})
		w := watch(ctx, f.owner.on(quick), f.org, f.run.GetId(), 0)
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))
		_, err := f.owner.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: f.org, RunId: f.run.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.nextEvent(2 * time.Second).GetKind()).To(Equal("run.canceled"))

		time.Sleep(300 * time.Millisecond) // the stream has sent the canceled snapshot and waits
		// The worker's cut-off item arrives after the run is terminal.
		_, err = store.New(e.pool).AppendCanceledRunEventOnce(ctx, store.AppendCanceledRunEventOnceParams{
			OrgID: run.OrgID, RunID: run.ID, Epoch: run.Epoch,
			Kind: "acp.agent_message_chunk", Payload: []byte(`{"content":{"type":"text","text":"half a sen"}}`), SourceSeq: 1000,
		})
		Expect(err).NotTo(HaveOccurred())

		var kinds []string
		var last *runv1alpha1.WatchRunResponse
		for msg := range w.items {
			if ev := msg.GetEvent(); ev != nil {
				kinds = append(kinds, ev.GetKind())
			}
			if msg.GetRun() != nil {
				last = msg
				kinds = append(kinds, "snapshot")
			}
		}
		Expect(<-w.done).To(Succeed())
		Expect(kinds).To(Equal([]string{"snapshot", "acp.agent_message_chunk", "snapshot"}))
		Expect(last.GetRun().GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_CANCELED))
	})

	It("ends at once on a run canceled longer ago than the grace (old issue 660)", func(ctx SpecContext) {
		quick := newEnvOn(ctx, e, envOptions{watchPoll: time.Hour, watchCancelGrace: 30 * time.Second})
		_, err := f.owner.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: f.org, RunId: f.run.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `UPDATE runs SET finished_at = now() - interval '1 hour' WHERE id = $1`, run.ID)
		Expect(err).NotTo(HaveOccurred())

		start := time.Now()
		w := watch(ctx, f.owner.on(quick), f.org, f.run.GetId(), 0)
		for range w.items {
		}
		Expect(<-w.done).To(Succeed())
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second), "an old canceled run does not hold the stream for the grace")
	})

	It("ends after the run finishes and its last event is sent", func(ctx SpecContext) {
		w := watch(ctx, f.owner, f.org, f.run.GetId(), 0)
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))
		_, err := f.owner.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: f.org, RunId: f.run.GetId()})
		Expect(err).NotTo(HaveOccurred())

		ev := w.nextEvent(time.Second)
		Expect(ev.GetKind()).To(Equal("run.canceled"))
		// The final message is a terminal snapshot that follows the last event (old issue 140):
		// the server reads events, then the run; a snapshot sent before the event is not enough.
		var last *runv1alpha1.WatchRunResponse
		for msg := range w.items {
			Expect(msg.GetEvent()).To(BeNil(), "no event after run.canceled")
			last = msg
		}
		Expect(<-w.done).To(Succeed())
		Expect(last).NotTo(BeNil(), "a run snapshot follows the last event")
		Expect(last.GetRun().GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_CANCELED))
	})

	It("sends the final state and closes when the run finishes, found by the backstop poll", func(ctx SpecContext) {
		// A short poll: the finish writes no event, so only the backstop poll can see it.
		quick := newEnvOn(ctx, e, envOptions{watchPoll: 50 * time.Millisecond})
		w := watch(ctx, f.owner.on(quick), f.org, f.run.GetId(), 0)
		Expect(w.nextEvent(5 * time.Second).GetKind()).To(Equal("run.queued"))
		ad.finish(ctx, run)
		var last *runv1alpha1.WatchRunResponse
		for msg := range w.items {
			if msg.GetRun() != nil {
				last = msg
			}
		}
		Expect(<-w.done).To(Succeed(), "the stream closes after the final state")
		Expect(last.GetRun().GetState().String()).To(Or(Equal("RUN_STATE_SUCCEEDED"), Equal("RUN_STATE_FAILED")))
	})

	It("sends a heartbeat on a quiet run, and none while events flow", func(ctx SpecContext) {
		quiet := newEnvOn(ctx, e, envOptions{watchPoll: time.Hour, watchHeartbeat: 500 * time.Millisecond})
		w := watch(ctx, f.owner.on(quiet), f.org, f.run.GetId(), 0)
		Eventually(func() int { return quiet.hub.Subscribers() }).Should(BeNumerically(">=", 1))

		// An event every 100 ms keeps resetting the 500 ms idle timer: 1.5 s without a heartbeat.
		var beats, events int
		count := func() {
			for {
				select {
				case msg := <-w.items:
					if msg.GetHeartbeat() != nil {
						beats++
					}
					if msg.GetEvent() != nil {
						events++
					}
				default:
					return
				}
			}
		}
		for i := 2; i < 17; i++ {
			ad.event(ctx, run, i)
			time.Sleep(100 * time.Millisecond)
			count()
		}
		Expect(events).To(BeNumerically(">", 0))
		Expect(beats).To(BeZero(), "no heartbeat while events flow")

		// Quiet now: the heartbeat arrives, and again after the next idle interval.
		got := 0
		timeout := time.After(5 * time.Second)
		for got < 2 {
			select {
			case msg := <-w.items:
				if msg.GetHeartbeat() != nil {
					got++
				}
			case <-timeout:
				Fail("no two heartbeats within 5s of going quiet")
			}
		}
	})

	It("wakes streams on every API replica when another process writes", func(ctx SpecContext) {
		replica := newEnvOn(ctx, e, envOptions{watchPoll: time.Hour})
		w1 := watch(ctx, f.owner, f.org, f.run.GetId(), 1)
		w2 := watch(ctx, f.owner.on(replica), f.org, f.run.GetId(), 1)
		Eventually(func() int { return e.hub.Subscribers() + replica.hub.Subscribers() }).Should(Equal(2))

		start := time.Now()
		ad.event(ctx, run, 2) // written by "agentd", neither replica
		Expect(w1.nextEvent(time.Second).GetSeq()).To(Equal(int64(2)))
		Expect(w2.nextEvent(time.Second).GetSeq()).To(Equal(int64(2)))
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})

	It("loses and duplicates nothing across reconnects to different replicas", func(ctx SpecContext) {
		replica := newEnvOn(ctx, e, envOptions{watchPoll: time.Hour})
		servers := []*client{f.owner, f.owner.on(replica)}
		const total = 150

		writerDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(writerDone)
			for i := 2; i <= total; i++ {
				ad.event(ctx, run, i)
				if i%10 == 0 {
					time.Sleep(5 * time.Millisecond)
				}
			}
			ad.finish(ctx, run)
		}()

		var seen []int64
		cursor := int64(0)
		for attempt := 0; ; attempt++ {
			Expect(attempt).To(BeNumerically("<", 100), "too many reconnects")
			w := watch(ctx, servers[attempt%2], f.org, f.run.GetId(), cursor)
			// Drop the stream after a few events, mid-flight.
			ended := false
			for got := 0; got < 7; {
				msg, ok := <-w.items
				if !ok {
					ended = true
					break
				}
				if ev := msg.GetEvent(); ev != nil {
					seen = append(seen, ev.GetSeq())
					cursor = ev.GetSeq()
					got++
				}
			}
			w.cancel()
			for range w.items {
			}
			if ended {
				Expect(<-w.done).To(Succeed())
				break
			}
		}
		<-writerDone

		want := make([]int64, 0, total)
		for i := int64(1); i <= total; i++ {
			want = append(want, i)
		}
		Expect(seen).To(Equal(want))
	})

	It("rejects a negative after_seq", func(ctx SpecContext) {
		w := watch(ctx, f.owner, f.org, f.run.GetId(), -1)
		for range w.items {
		}
		Expect(codeOf(<-w.done)).To(Equal(connect.CodeInvalidArgument))
	})
})
