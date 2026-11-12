// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// Drain on SIGTERM (ADR 0085 decision 2, old issue 403): a stopping pool claims nothing more, lets the turn
// in progress finish up to the drain deadline, and hands a run still going at the deadline off.
var _ = Describe("Draining on stop", func() {
	// stopAsync stops the pool without blocking the spec: a draining pool returns only when its
	// turns are done.
	stopAsync := func(stop func()) {
		go func() {
			defer GinkgoRecover()
			stop()
		}()
	}

	It("finishes the turn in progress and claims nothing more", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		var (
			started = make(chan struct{})
			release = make(chan struct{})
			once    sync.Once
		)
		e.harness.mu.Lock()
		e.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			once.Do(func() { close(started) })
			select {
			case <-release:
				return "finished during the drain", true
			case <-ctx.Done():
				return "", false
			}
		}
		e.harness.mu.Unlock()

		first := e.enqueue(ctx, e.task(ctx, "first"))
		second := e.enqueue(ctx, e.task(ctx, "second")) // same agent: waits for the first to end
		stop := e.pool("agentd-a", func(c *agentd.Config) { c.DrainDeadline = 30 * time.Second })
		Eventually(started).WithTimeout(racescale.D(10 * time.Second)).Should(BeClosed())
		claimed := getRun(ctx, first)

		stopAsync(stop)
		// The pool is draining: the turn is not cut off, and the lease is renewed past its 2 s.
		Consistently(runState(ctx, first)).WithTimeout(3 * time.Second).Should(Equal(store.RunStateRunning))
		Expect(getRun(ctx, first).LeaseExpiresAt.Time).To(BeTemporally(">", time.Now()), "heartbeats go on while draining")

		close(release)
		Eventually(runState(ctx, first)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(getRun(ctx, first).Epoch).To(Equal(claimed.Epoch))
		Expect(getRun(ctx, first).Takeovers).To(BeZero())
		Consistently(runState(ctx, second)).WithTimeout(time.Second).Should(Equal(store.RunStateQueued),
			"a stopping pool claims no new run")
	})

	It("hands the run off at the drain deadline, and another replica takes it over", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		r := e.enqueue(ctx, e.task(ctx, markHang))
		stop := e.pool("agentd-a", func(c *agentd.Config) { c.DrainDeadline = time.Second })
		Eventually(e.harness.hangs).WithTimeout(racescale.D(10 * time.Second)).Should(Receive())
		claimed := getRun(ctx, r)

		started := time.Now()
		stop() // returns once the deadline passed and the run was released
		Expect(time.Since(started)).To(BeNumerically(">=", 900*time.Millisecond), "the turn got the drain deadline")
		Expect(getRun(ctx, r).LeaseExpiresAt.Time).To(BeTemporally("<", time.Now()), "the lease was released")

		e.pool("agentd-b")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		final := getRun(ctx, r)
		Expect(final.OwnerID.String).To(HavePrefix("agentd-b/"))
		Expect(final.Epoch).To(Equal(claimed.Epoch + 1))
	})

	It("hands a run off at once when it has not begun its turn", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.recordSandbox(ctx, store.SandboxStateStopped, "") // the run waits for its sandbox
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		stop := e.pool("agentd-a", func(c *agentd.Config) { c.DrainDeadline = time.Minute })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))

		started := time.Now()
		stop() // fails the spec if the pool does not stop within 10 s: the deadline is a minute
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
		Expect(getRun(ctx, r).LeaseExpiresAt.Time).To(BeTemporally("<", time.Now()), "the lease was released")
		Expect(kinds(events(ctx, r))).NotTo(ContainElement("run.interrupted"), "a handed-off run is not reported as interrupted")
	})

	It("stops a retry back-off at once when the run is cancelled during the drain", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		srv, err := fakemodel.New(fakemodel.Script{
			Model: "faulty", Faults: fakemodel.Faults{RateLimit: 5, RetryAfter: 30},
			Turns: []fakemodel.Turn{{Text: "one two three four"}},
		})
		Expect(err).NotTo(HaveOccurred())
		ts := httptest.NewServer(srv)
		DeferCleanup(ts.Close)
		e.harness.modelFault = modelCaller(ts.URL)

		r := e.enqueue(ctx, e.task(ctx, "hello"))
		// agentd's own retry layer, the one for a harness whose harness does not retry.
		stop := e.pool("agentd-a", agentd.OwnRetryLayer, func(c *agentd.Config) { c.DrainDeadline = time.Minute })
		Eventually(func() int { return len(srv.Requests()) }).WithTimeout(racescale.D(15 * time.Second)).Should(BeNumerically(">=", 1))
		Eventually(func() bool {
			for _, ev := range events(ctx, r) {
				if ev.Kind == "run.retry" {
					return true
				}
			}
			return false
		}).WithTimeout(racescale.D(10*time.Second)).Should(BeTrue(), "the run is in its retry back-off")

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			stop()
		}()
		time.Sleep(500 * time.Millisecond) // the pool is draining, the turn has begun
		_, err = db.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'cancelled in the drain',
			finished_at = now(), ready_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL
			WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(done).WithTimeout(racescale.D(15*time.Second)).Should(BeClosed(),
			"the fence ends the back-off instead of sleeping out the 30 s Retry-After")
	})
})
