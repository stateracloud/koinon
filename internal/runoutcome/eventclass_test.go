// SPDX-License-Identifier: Apache-2.0

package runoutcome_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// flaky is the pool, except that AppendRunEventOnce fails with err for its next `fails` calls
// (every call while fails is negative). attempts counts the calls it failed, calls all of them. With committed, a
// failing call inserts the event first and then reports err (the reply of a committed insert
// lost); with hang, it waits for its context to end and reports the context's error.
type flaky struct {
	store.DBTX
	err       error
	committed bool
	hang      bool
	fails     atomic.Int64
	attempts  atomic.Int64
	calls     atomic.Int64 // every AppendRunEventOnce, failed or not
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

// committedRow scans the real row and then reports err.
type committedRow struct {
	pgx.Row
	err error
}

func (r committedRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	return r.err
}

func (f *flaky) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: AppendRunEventOnce ") {
		f.calls.Add(1)
		if n := f.fails.Load(); n != 0 {
			f.fails.Add(-1)
			f.attempts.Add(1)
			switch {
			case f.committed:
				return committedRow{f.DBTX.QueryRow(ctx, sql, args...), f.err}
			case f.hang:
				<-ctx.Done()
				return failedRow{ctx.Err()}
			}
			return failedRow{f.err}
		}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

// flakyOwner replaces the world's Owner with one whose event writes go through f.
func (w *world) flakyOwner(f *flaky) {
	f.DBTX = pool
	w.own = runoutcome.New(runoutcome.Config{DB: pool, Queries: store.New(f), Run: w.run, Metrics: w.metrics})
}

// transient is a serialization failure: the database refused for now.
var transient = &pgconn.PgError{Code: "40001", Message: "could not serialize access"}

const (
	diagnosticKind = "acp.agent_thought_chunk"
	businessKind   = "run.interrupted"
)

var _ = Describe("Run event classes", func() {
	It("drops a diagnostic event whose write keeps failing, counts it, and the run goes on", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		f := &flaky{err: transient}
		f.fails.Store(-1)
		w.flakyOwner(f)

		Expect(w.own.Record(ctx, diagnosticKind, map[string]any{"text": "thinking"})).To(Succeed())
		Expect(f.attempts.Load()).To(Equal(int64(2)), "a diagnostic event is tried twice")
		Expect(w.own.IsFenced()).To(BeFalse())
		Expect(w.metrics.writeFailed).To(Equal([]string{"diagnostic/transient"}))

		By("the next write goes through, under the next source seq")
		f.fails.Store(0)
		Expect(w.own.Record(ctx, businessKind, map[string]any{})).To(Succeed())
		Expect(w.runKinds(ctx)).To(Equal([]string{businessKind}))
		Expect(w.current(ctx).State).To(Equal(store.RunStateRunning))
	})

	It("abandons the run when a business event's write keeps failing, as before", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		f := &flaky{err: transient}
		f.fails.Store(-1)
		w.flakyOwner(f)

		err := w.own.Record(ctx, businessKind, map[string]any{})
		Expect(err).To(MatchError(transient))
		Expect(f.attempts.Load()).To(Equal(int64(5)), "a business event is tried five times")
		Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceAbandon))
		Expect(w.metrics.writeFailed).To(Equal([]string{"business/transient"}))
		Expect(w.own.Record(ctx, diagnosticKind, map[string]any{})).To(MatchError(runoutcome.ErrFenced),
			"an abandoned run writes nothing more, diagnostic events included")
	})

	It("records a diagnostic event that fails once and then goes through, and counts nothing", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		f := &flaky{err: transient}
		f.fails.Store(1)
		w.flakyOwner(f)

		Expect(w.own.Record(ctx, diagnosticKind, map[string]any{})).To(Succeed())
		Expect(w.runKinds(ctx)).To(Equal([]string{diagnosticKind}))
		Expect(w.metrics.writeFailed).To(BeEmpty())
	})

	It("takes a unique violation for the event stored already, and counts nothing", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		f := &flaky{err: &pgconn.PgError{Code: "23505"}, committed: true}
		f.fails.Store(1)
		w.flakyOwner(f)

		Expect(w.own.Record(ctx, businessKind, map[string]any{})).To(Succeed())
		Expect(f.calls.Load()).To(Equal(int64(1)), "no retry: the event is there")
		Expect(w.runKinds(ctx)).To(Equal([]string{businessKind}))
		Expect(w.own.IsFenced()).To(BeFalse())
		Expect(w.metrics.writeFailed).To(BeEmpty())
	})

	It("gives each try of a diagnostic write its own deadline, so a hung connection holds the run up briefly", func(ctx SpecContext) {
		DeferCleanup(runoutcome.SetDiagnosticTryTimeout(50 * time.Millisecond))
		w := newWorld(ctx, true)
		f := &flaky{hang: true}
		f.fails.Store(-1)
		w.flakyOwner(f)

		start := time.Now()
		Expect(w.own.Record(ctx, diagnosticKind, map[string]any{})).To(Succeed())
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second), "two tries of 50 ms and a 100 ms pause")
		Expect(f.attempts.Load()).To(Equal(int64(2)))
		Expect(w.own.IsFenced()).To(BeFalse())
		Expect(w.metrics.writeFailed).To(Equal([]string{"diagnostic/timeout"}))
	})

	DescribeTable("fences either class when the run is no longer the worker's",
		func(ctx SpecContext, kind string) {
			w := newWorld(ctx, true)
			_, err := w.cancelUnowned(ctx, runoutcome.Cancellation{Reason: runoutcome.ReasonUser, Message: "stop", Actor: runoutcome.System})
			Expect(err).NotTo(HaveOccurred())

			Expect(w.own.Record(ctx, kind, map[string]any{})).To(MatchError(runoutcome.ErrFenced))
			Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
			Expect(w.metrics.writeFailed).To(BeEmpty(), "a fence is not a failed write")
		},
		Entry("a diagnostic event", diagnosticKind),
		Entry("a business event", businessKind),
	)

	DescribeTable("counts why a diagnostic write failed, from a fixed set",
		func(ctx SpecContext, err error, reason string) {
			w := newWorld(ctx, true)
			f := &flaky{err: err}
			f.fails.Store(-1)
			w.flakyOwner(f)

			Expect(w.own.Record(ctx, diagnosticKind, map[string]any{})).To(Succeed())
			Expect(w.metrics.writeFailed).To(Equal([]string{"diagnostic/" + reason}))
		},
		Entry("a serialization failure", transient, "transient"),
		Entry("a deadlock", &pgconn.PgError{Code: "40P01"}, "transient"),
		Entry("too many connections", &pgconn.PgError{Code: "53300"}, "transient"),
		Entry("an administrator's restart", &pgconn.PgError{Code: "57P01"}, "transient"),
		Entry("a connection failure reported by the server", &pgconn.PgError{Code: "08006"}, "conn"),
		Entry("a refused connection", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "conn"),
		Entry("a deadline", context.DeadlineExceeded, "timeout"),
		Entry("a statement timeout", &pgconn.PgError{Code: "57014"}, "timeout"),
		Entry("a canceled context", context.Canceled, "canceled"),
		Entry("a canceled context, wrapped as pgx reports it", fmt.Errorf("timeout: %w", context.Canceled), "canceled"),
		Entry("a constraint violation", &pgconn.PgError{Code: "23505"}, "other"),
		Entry("anything else", errors.New("the database said no"), "other"),
	)

	DescribeTable("PermanentSQLState tells an error that cannot succeed on retry (koinon#123)",
		func(err error, code string, permanent bool) {
			got, ok := runoutcome.PermanentSQLState(err)
			Expect(ok).To(Equal(permanent))
			Expect(got).To(Equal(code))
		},
		Entry("a cardinality violation", &pgconn.PgError{Code: "21000"}, "21000", true),
		Entry("a cardinality violation, wrapped", fmt.Errorf("upsert: %w", &pgconn.PgError{Code: "21000"}), "21000", true),
		Entry("a unique violation", &pgconn.PgError{Code: "23505"}, "23505", true),
		Entry("a data exception", &pgconn.PgError{Code: "22P02"}, "22P02", true),
		Entry("a syntax error", &pgconn.PgError{Code: "42601"}, "42601", true),
		Entry("a serialization failure", &pgconn.PgError{Code: "40001"}, "", false),
		Entry("a connection failure", &pgconn.PgError{Code: "08006"}, "", false),
		Entry("a statement timeout", &pgconn.PgError{Code: "57014"}, "", false),
		Entry("a canceled context", context.Canceled, "", false),
		Entry("an error that is not the database's", errors.New("boom"), "", false),
	)

	It("pins the class of every known kind; an unknown kind is business", func() {
		B, D := runoutcome.ClassBusiness, runoutcome.ClassDiagnostic
		Expect(runoutcome.KnownEventKinds()).To(Equal(map[string]runoutcome.EventClass{
			"run.succeeded": B, "run.failed": B, "run.canceled": B, "run.parked": B, "run.requeued": B,
			"run.session": B, "run.session_reset": B, "run.session_restarted": B, "run.prompt": B,
			"run.interrupted": B, "acp.permission": B,
			"acp.agent_message_chunk": B, "acp.tool_call": B, "acp.tool_call_update": B,

			"acp.agent_thought_chunk": D, "acp.user_message_chunk": D, "acp.plan": D,
			"acp.available_commands_update": D, "acp.current_mode_update": D, "acp.usage_update": D,
			"run.uncached": D, "run.sandbox_swept": D, "run.home_restored": D,
			"run.instructions_restored": D, "run.mcp_server_missing": D, "run.mcp_server_recovered": D,
			"run.waiting_for_model": D, "run.refusal": D, "run.retry": D, "run.loop_nudge": D, "run.wrap_up_notice": D,
			"run.summary": D, "run.summary_failed": D, "run.summary_refused": D,
		}))
		Expect(runoutcome.ClassOf("acp.something_new")).To(Equal(B))
		Expect(runoutcome.ClassOf("run.something_new")).To(Equal(B))
	})
})
