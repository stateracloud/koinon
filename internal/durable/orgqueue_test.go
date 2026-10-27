// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
)

// itemLog records when items of each org start and finish, and how many of an org run at once.
type itemLog struct {
	mu      sync.Mutex
	started map[string][]time.Time
	ids     map[string]map[string]bool
	done    map[string]int
	active  map[string]int
	peak    map[string]int
}

func newItemLog() *itemLog {
	return &itemLog{started: map[string][]time.Time{}, ids: map[string]map[string]bool{}, done: map[string]int{}, active: map[string]int{}, peak: map[string]int{}}
}

func (l *itemLog) begin(org, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids[org] == nil {
		l.ids[org] = map[string]bool{}
	}
	l.ids[org][id] = true
	l.started[org] = append(l.started[org], time.Now())
	l.active[org]++
	l.peak[org] = max(l.peak[org], l.active[org])
}

func (l *itemLog) end(org string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active[org]--
	l.done[org]++
}

func (l *itemLog) starts(org string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.started[org])
}

// distinct counts different items that started, so a re-execution does not count twice.
func (l *itemLog) distinct(org string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ids[org])
}

func (l *itemLog) finished(org string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done[org]
}

func (l *itemLog) peakOf(org string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peak[org]
}

type item struct{ Org, ID string }

func newOrgRuntime(dsn string, log *itemLog, work time.Duration, cfg durable.OrgQueueConfig) (*durable.Runtime, *durable.OrgWork) {
	rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1", ExecutorID: "koinon-api-test"})
	run := func(_ dbos.Context, in item) (string, error) {
		log.begin(in.Org, in.ID)
		defer log.end(in.Org)
		time.Sleep(work)
		return in.Org, nil
	}
	Expect(rt.Register(func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, run, dbos.WithWorkflowName("test.org-item"))
		return nil
	})).To(Succeed())
	cfg.PollingInterval = 200 * time.Millisecond
	w, err := durable.NewOrgWork(rt, cfg)
	Expect(err).NotTo(HaveOccurred())
	return rt, w
}

var itemSeq atomic.Int64

func enqueueItem(ctx context.Context, w *durable.OrgWork, org string) dbos.WorkflowHandle[string] {
	GinkgoHelper()
	h, err := durable.EnqueueForOrg[string](ctx, w, org, "test.org-item", item{Org: org, ID: fmt.Sprintf("%s-%d", org, itemSeq.Add(1))})
	Expect(err).NotTo(HaveOccurred())
	return h
}

var _ = Describe("per-org queue", func() {
	It("does not let one org's burst delay another org's work, and holds each org to its concurrency", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		log := newItemLog()
		rt, w := newOrgRuntime(dsn, log, 500*time.Millisecond, durable.OrgQueueConfig{
			Defaults:   durable.OrgLimits{Concurrency: 2, RatePerWindow: 1000},
			RateWindow: time.Minute,
		})
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		for range 50 {
			enqueueItem(ctx, w, "org-a")
		}
		begin := time.Now()
		hb := enqueueItem(ctx, w, "org-b")
		_, err := hb.GetResult()
		Expect(err).NotTo(HaveOccurred())
		// 50 items at 2 at a time, 0.5s each, take about 12s; org B must not wait for them.
		Expect(time.Since(begin)).To(BeNumerically("<", 4*time.Second))
		Expect(log.finished("org-a")).To(BeNumerically("<", 40), "org A is still working through its backlog")

		Eventually(func() int { return log.finished("org-a") }, 60*time.Second, 200*time.Millisecond).Should(Equal(50))
		Expect(log.peakOf("org-a")).To(Equal(2))
	}, SpecTimeout(3*time.Minute))

	It("gives an org with its own limits a higher concurrency than the default", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		log := newItemLog()
		rt, w := newOrgRuntime(dsn, log, 400*time.Millisecond, durable.OrgQueueConfig{
			Defaults: durable.OrgLimits{Concurrency: 1, RatePerWindow: 1000},
			Limits: func(_ context.Context, org string) (durable.OrgLimits, error) {
				if org == "org-big" {
					return durable.OrgLimits{Concurrency: 5}, nil
				}
				return durable.OrgLimits{}, nil
			},
		})
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		var handles []dbos.WorkflowHandle[string]
		for range 10 {
			handles = append(handles, enqueueItem(ctx, w, "org-big"), enqueueItem(ctx, w, "org-small"))
		}
		for _, h := range handles {
			_, err := h.GetResult()
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(log.peakOf("org-small")).To(Equal(1))
		Expect(log.peakOf("org-big")).To(BeNumerically(">", 1))
		Expect(log.peakOf("org-big")).To(BeNumerically("<=", 5))
	}, SpecTimeout(3*time.Minute))

	It("holds a rate limit across a restart of the runtime in the same window", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		log := newItemLog()
		cfg := durable.OrgQueueConfig{
			Defaults:   durable.OrgLimits{Concurrency: 10, RatePerWindow: 3},
			RateWindow: 12 * time.Second,
		}
		first, w := newOrgRuntime(dsn, log, 50*time.Millisecond, cfg)
		Expect(first.Launch(ctx)).To(Succeed())
		window := time.Now()
		for range 6 {
			enqueueItem(ctx, w, "org-r")
		}
		Eventually(func() int { return log.starts("org-r") }, 8*time.Second, 100*time.Millisecond).Should(Equal(3))
		// Let the three running items finish: DBOS puts in-flight workflows back to ENQUEUED on
		// shutdown, and the second runtime would run them again.
		Eventually(func() int { return log.finished("org-r") }, 8*time.Second, 50*time.Millisecond).Should(Equal(3))

		// Stop and restart against the same database, still inside the window.
		Expect(first.Shutdown(5 * time.Second)).To(Succeed())
		second, _ := newOrgRuntime(dsn, log, 50*time.Millisecond, cfg)
		Expect(second.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = second.Shutdown(5 * time.Second) })

		// The restart took place well inside the window: the rest must wait for it to end.
		Expect(time.Since(window)).To(BeNumerically("<", 8*time.Second))
		Consistently(func() int { return log.distinct("org-r") }, 2*time.Second, 100*time.Millisecond).Should(Equal(3))

		Eventually(func() int { return log.distinct("org-r") }, 30*time.Second, 200*time.Millisecond).Should(Equal(6))
		Expect(log.starts("org-r")).To(Equal(6), "no item ran twice")
		Expect(time.Since(window)).To(BeNumerically(">=", 12*time.Second-time.Second), "the other three started only after the window moved on")
	}, SpecTimeout(3*time.Minute))

	It("rejects work enqueued before launch or without an org", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		log := newItemLog()
		rt, w := newOrgRuntime(dsn, log, time.Millisecond, durable.OrgQueueConfig{})
		_, err := durable.EnqueueForOrg[string](ctx, w, "org-a", "test.org-item", item{Org: "org-a"})
		Expect(err).To(HaveOccurred())
		Expect(rt.Shutdown(time.Second)).To(Succeed())
		_, err = durable.EnqueueForOrg[string](ctx, w, "", "test.org-item", item{})
		Expect(err).To(HaveOccurred())
	})
})
