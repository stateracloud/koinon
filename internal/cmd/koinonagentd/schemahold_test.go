// SPDX-License-Identifier: Apache-2.0

package koinonagentd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/schemagate"
)

// switchSchema is the schema a spec sets between gate checks.
type switchSchema struct {
	mu sync.Mutex
	s  schemagate.Schema
}

func (f *switchSchema) set(s schemagate.Schema) { f.mu.Lock(); f.s = s; f.mu.Unlock() }

func (f *switchSchema) read(context.Context) (schemagate.Schema, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

var _ = Describe("Schema gate on the worker pool (issue 680)", func() {
	var (
		schema *switchSchema
		gate   *schemagate.Gate
		claims atomic.Int64
		log    = slog.New(slog.NewTextHandler(io.Discard, nil))
	)
	// pool stands in for agentd.Pool.Run: it claims a run every millisecond until ctx ends, as the
	// workers do, and then returns as a drained pool does.
	pool := func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Millisecond):
				claims.Add(1)
			}
		}
	}
	BeforeEach(func() {
		claims.Store(0)
		schema = &switchSchema{}
		gate = schemagate.NewFunc(schema.read, schemagate.Range{Min: 10, Latest: 12}, log)
	})

	It("claims no run before the migration Job has run, then starts", func(ctx SpecContext) {
		schema.set(schemagate.Schema{Version: 9})
		Expect(gate.Check(ctx)).To(HaveOccurred())
		hctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- holdWork(hctx, gate, pool, log) }()
		Consistently(claims.Load, 100*time.Millisecond).Should(BeZero())

		schema.set(schemagate.Schema{Version: 12})
		Expect(gate.Check(ctx)).To(Succeed())
		Eventually(claims.Load).Should(BeNumerically(">", 0))
		stop() // the work must be gone before the next spec resets the counter
		Eventually(done).Should(Receive(BeNil()))
	})

	It("stops claiming and exits with an error when an older release meets a newer schema", func(ctx SpecContext) {
		schema.set(schemagate.Schema{Version: 12})
		Expect(gate.Check(ctx)).To(Succeed())
		done := make(chan error, 1)
		go func() { done <- holdWork(ctx, gate, pool, log) }()
		Eventually(claims.Load).Should(BeNumerically(">", 0))

		schema.set(schemagate.Schema{Version: 14, MinReader: 13})
		Expect(gate.Check(ctx)).To(HaveOccurred())
		var err error
		Eventually(done).Should(Receive(&err))
		Expect(errors.Is(err, schemagate.ErrUnsupported)).To(BeTrue(), "got %v", err)
		stopped := claims.Load()
		Consistently(claims.Load, 50*time.Millisecond).Should(Equal(stopped))
	})

	It("returns nil when agentd stops while it waits for the gate", func(ctx SpecContext) {
		schema.set(schemagate.Schema{Version: 9})
		Expect(gate.Check(ctx)).To(HaveOccurred())
		sctx, sigterm := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- holdWork(sctx, gate, pool, log) }()
		sigterm()
		Eventually(done).Should(Receive(BeNil()))
		Expect(claims.Load()).To(BeZero())
	})
})
