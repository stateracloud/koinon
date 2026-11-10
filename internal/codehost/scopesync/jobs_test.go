// SPDX-License-Identifier: Apache-2.0

package scopesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

func testConn(b byte) store.Connection {
	return store.Connection{
		OrgID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		ID:    pgtype.UUID{Bytes: [16]byte{b}, Valid: true},
	}
}

// fakeReader records readings and blocks each until released or its context ends.
type fakeReader struct {
	mu         sync.Mutex
	started    chan store.Connection
	release    chan struct{}
	active     map[byte]int
	maxSeen    map[byte]int
	total      map[byte]int
	canceled   int
	requestIDs []string
	actors     []audit.Actor
	names      []string
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		started: make(chan store.Connection, 32), release: make(chan struct{}, 32),
		active: map[byte]int{}, maxSeen: map[byte]int{}, total: map[byte]int{},
	}
}

func (f *fakeReader) run(ctx context.Context, row store.Connection, actor audit.Actor) {
	k := row.ID.Bytes[0]
	f.mu.Lock()
	f.requestIDs = append(f.requestIDs, audit.RequestID(ctx))
	f.actors = append(f.actors, actor)
	f.names = append(f.names, row.DisplayName)
	f.active[k]++
	f.total[k]++
	if f.active[k] > f.maxSeen[k] {
		f.maxSeen[k] = f.active[k]
	}
	f.mu.Unlock()
	f.started <- row
	select {
	case <-f.release:
	case <-ctx.Done():
		f.mu.Lock()
		f.canceled++
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.active[k]--
	f.mu.Unlock()
}

var _ = Describe("the scope readings queue", func() {
	var (
		f *fakeReader
		j *jobs
	)
	BeforeEach(func() {
		f = newFakeReader()
		j = newJobs(slog.Default(), f.run)
	})
	AfterEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		j.drain(ctx)
	})

	It("runs at most one reading per connection and coalesces a burst into one more", func() {
		for range 10 {
			j.start(context.Background(), testConn(1), audit.Actor{})
		}
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		j.drain(context.Background())
		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.maxSeen[1]).To(Equal(1))
		Expect(f.total[1]).To(Equal(2))
	})

	It("carries the request ID of the request that queued a reading, the newest when requests coalesce", func() {
		j.start(audit.WithRequestID(context.Background(), "req-1"), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		j.start(audit.WithRequestID(context.Background(), "req-2"), testConn(1), audit.Actor{})
		j.start(audit.WithRequestID(context.Background(), "req-3"), testConn(1), audit.Actor{})
		f.release <- struct{}{}
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		j.drain(context.Background())
		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.requestIDs).To(Equal([]string{"req-1", "req-3"}))
	})

	It("keeps a person's pending request whole when the periodic refresh arrives, and lets a person replace the refresh's", func() {
		periodic := audit.System("koinon-api:code-host-scope")
		person := audit.User(pgtype.UUID{Bytes: [16]byte{9}, Valid: true})
		named := func(name string) store.Connection {
			row := testConn(1)
			row.DisplayName = name
			return row
		}

		By("a person's request waits behind a running reading, and the refresh's request joins it")
		j.start(context.Background(), named("running"), periodic)
		Eventually(f.started).Should(Receive())
		j.start(audit.WithRequestID(context.Background(), "req-person"), named("person's row"), person)
		refresh := j.start(context.Background(), named("refresh's row"), periodic)
		f.release <- struct{}{}
		Eventually(f.started).Should(Receive())
		Expect(refresh).NotTo(BeClosed(), "the refresh waits for the reading that covers it")
		f.release <- struct{}{}
		Eventually(refresh).Should(BeClosed())

		By("the refresh's request waits, and a person's request replaces it")
		j.start(context.Background(), named("running"), periodic)
		Eventually(f.started).Should(Receive())
		j.start(context.Background(), named("refresh's row"), periodic)
		j.start(audit.WithRequestID(context.Background(), "req-person-2"), named("newer person's row"), person)
		f.release <- struct{}{}
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		j.drain(context.Background())

		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.names).To(Equal([]string{"running", "person's row", "running", "newer person's row"}))
		Expect(f.actors[1]).To(Equal(person))
		Expect(f.requestIDs[1]).To(Equal("req-person"))
		Expect(f.actors[3]).To(Equal(person))
		Expect(f.requestIDs[3]).To(Equal("req-person-2"))
	})

	It("does not carry the cancellation of the request, and Drain still cancels the reading", func() {
		reqCtx, cancelReq := context.WithCancel(audit.WithRequestID(context.Background(), "req-1"))
		j.start(reqCtx, testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		cancelReq()
		Consistently(func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.canceled }, 100*time.Millisecond).Should(BeZero())
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		j.drain(ctx)
		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.canceled).To(Equal(1))
	})

	It("reads a periodic refresh and a request for one connection one at a time, and tells each when it is done", func() {
		periodic := j.start(context.Background(), testConn(1), audit.System("koinon-api:code-host-scope"))
		Eventually(f.started).Should(Receive())
		manual := j.start(audit.WithRequestID(context.Background(), "req-1"), testConn(1), audit.Actor{})
		Consistently(f.started, 100*time.Millisecond).ShouldNot(Receive(), "no second reading while the first runs")
		Expect(periodic).NotTo(BeClosed())
		f.release <- struct{}{}
		Eventually(periodic).Should(BeClosed())
		Expect(manual).NotTo(BeClosed())
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		Eventually(manual).Should(BeClosed())
		j.drain(context.Background())
		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.maxSeen[1]).To(Equal(1))
	})

	It("closes the channel of a request coalesced away once the covering reading ends", func() {
		j.start(context.Background(), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		replaced := j.start(context.Background(), testConn(1), audit.Actor{})
		j.start(context.Background(), testConn(1), audit.Actor{})
		f.release <- struct{}{}
		Eventually(f.started).Should(Receive())
		Expect(replaced).NotTo(BeClosed())
		f.release <- struct{}{}
		Eventually(replaced).Should(BeClosed())
		j.drain(context.Background())
	})

	It("returns a closed channel after drain", func() {
		j.drain(context.Background())
		Expect(j.start(context.Background(), testConn(1), audit.Actor{})).To(BeClosed())
	})

	It("reads different connections side by side", func() {
		j.start(context.Background(), testConn(1), audit.Actor{})
		j.start(context.Background(), testConn(2), audit.Actor{})
		Eventually(f.started).Should(Receive())
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		f.release <- struct{}{}
		j.drain(context.Background())
	})

	It("reads a connection again after its reading ended", func() {
		j.start(context.Background(), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		Eventually(func() int { j.mu.Lock(); defer j.mu.Unlock(); return len(j.running) }).Should(BeZero())
		j.start(context.Background(), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		j.drain(context.Background())
	})

	It("drain waits for a running reading", func() {
		j.start(context.Background(), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		done := make(chan struct{})
		go func() { j.drain(context.Background()); close(done) }()
		Consistently(done, 100*time.Millisecond).ShouldNot(BeClosed())
		f.release <- struct{}{}
		Eventually(done).Should(BeClosed())
	})

	It("drain cancels a reading that outlasts its deadline and returns", func() {
		j.start(context.Background(), testConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan struct{})
		go func() { j.drain(ctx); close(done) }()
		Eventually(done).Should(BeClosed())
		f.mu.Lock()
		defer f.mu.Unlock()
		Expect(f.canceled).To(Equal(1))
	})

	It("starts no reading after drain", func() {
		j.drain(context.Background())
		j.start(context.Background(), testConn(1), audit.Actor{})
		Consistently(f.started, 100*time.Millisecond).ShouldNot(Receive())
	})
})

var _ = Describe("logging a failed reading", func() {
	logged := func(ctx context.Context, err error) string {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		(&Syncer{log: log}).logReadFailure(ctx, testConn(1), err)
		return buf.String()
	}
	It("warns about a real failure", func() {
		Expect(logged(context.Background(), errors.New("boom"))).To(ContainSubstring("level=WARN"))
	})
	It("warns about the per-reading timeout, with the connection's ID", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		out := logged(ctx, context.DeadlineExceeded)
		Expect(out).To(ContainSubstring("level=WARN"))
		Expect(out).To(ContainSubstring(testConn(1).ID.String()))
	})
	It("does not warn about a reading Drain cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out := logged(ctx, fmt.Errorf("list: %w", context.Canceled))
		Expect(out).NotTo(ContainSubstring("level=WARN"))
	})
	It("warns about a Canceled error while the context is still live", func() {
		Expect(logged(context.Background(), context.Canceled)).To(ContainSubstring("level=WARN"))
	})
})
