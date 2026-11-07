// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

func scopeTestConn(b byte) store.Connection {
	return store.Connection{
		OrgID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		ID:    pgtype.UUID{Bytes: [16]byte{b}, Valid: true},
	}
}

// fakeReader records readings and blocks each until released or its context ends.
type fakeReader struct {
	mu       sync.Mutex
	started  chan store.Connection
	release  chan struct{}
	active   map[byte]int
	maxSeen  map[byte]int
	total    map[byte]int
	canceled int
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		started: make(chan store.Connection, 32), release: make(chan struct{}, 32),
		active: map[byte]int{}, maxSeen: map[byte]int{}, total: map[byte]int{},
	}
}

func (f *fakeReader) run(ctx context.Context, row store.Connection, _ audit.Actor) {
	k := row.ID.Bytes[0]
	f.mu.Lock()
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

var _ = Describe("scopeJobs", func() {
	var (
		f *fakeReader
		j *scopeJobs
	)
	BeforeEach(func() {
		f = newFakeReader()
		j = newScopeJobs(slog.Default(), f.run)
	})
	AfterEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		j.drain(ctx)
	})

	It("runs at most one reading per connection and coalesces a burst into one more", func() {
		for range 10 {
			j.start(scopeTestConn(1), audit.Actor{})
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

	It("reads different connections side by side", func() {
		j.start(scopeTestConn(1), audit.Actor{})
		j.start(scopeTestConn(2), audit.Actor{})
		Eventually(f.started).Should(Receive())
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		f.release <- struct{}{}
		j.drain(context.Background())
	})

	It("reads a connection again after its reading ended", func() {
		j.start(scopeTestConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		Eventually(func() int { j.mu.Lock(); defer j.mu.Unlock(); return len(j.running) }).Should(BeZero())
		j.start(scopeTestConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		f.release <- struct{}{}
		j.drain(context.Background())
	})

	It("drain waits for a running reading", func() {
		j.start(scopeTestConn(1), audit.Actor{})
		Eventually(f.started).Should(Receive())
		done := make(chan struct{})
		go func() { j.drain(context.Background()); close(done) }()
		Consistently(done, 100*time.Millisecond).ShouldNot(BeClosed())
		f.release <- struct{}{}
		Eventually(done).Should(BeClosed())
	})

	It("drain cancels a reading that outlasts its deadline and returns", func() {
		j.start(scopeTestConn(1), audit.Actor{})
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
		j.start(scopeTestConn(1), audit.Actor{})
		Consistently(f.started, 100*time.Millisecond).ShouldNot(Receive())
	})
})
