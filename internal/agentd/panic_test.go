// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// panicBackend panics on the first Exec and delegates after that.
type panicBackend struct {
	agentd.Backend
	panicked atomic.Bool
}

func (b *panicBackend) Exec(ctx context.Context, ws, sb string, req execution.ExecRequest) (execution.Process, error) {
	if b.panicked.CompareAndSwap(false, true) {
		panic("fake backend: kaboom")
	}
	return b.Backend.Exec(ctx, ws, sb, req)
}

func (b *panicBackend) Upload(ctx context.Context, ws, sb, dest string, tar io.Reader) error {
	return b.Backend.Upload(ctx, ws, sb, dest, tar)
}

var _ = Describe("A panic while driving a run", func() {
	It("stops the heartbeat, fails the run, and leaves the worker driving the next one", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.backendOverride = &panicBackend{Backend: e.backend}
		first := e.enqueue(ctx, e.task(ctx, "first"))
		e.pool("agentd-a")

		// conclude runs after the heartbeat is stopped and waited for: a run that is failed has no
		// goroutine renewing its lease.
		Eventually(runState(ctx, first)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, first)
		Expect(got.OutcomeReason.String).To(ContainSubstring("panicked"))
		Expect(got.LeaseExpiresAt.Valid).To(BeFalse(), "no live lease is left behind")

		second := e.enqueue(ctx, e.task(ctx, "second"))
		Eventually(runState(ctx, second)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})
})
