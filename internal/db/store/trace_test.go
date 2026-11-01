// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Run traces (ADR 0074): the traced decision of a run and the turns with their trace IDs.
var _ = Describe("run traces", func() {
	It("stores a run's traced decision once", func(ctx SpecContext) {
		f := newFixture(ctx)
		r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		got, err := q.GetRunTraced(ctx, store.GetRunTracedParams{OrgID: f.org, ID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Valid).To(BeFalse(), "undecided until agentd claims the run")

		traced, err := q.DecideRunTraced(ctx, store.DecideRunTracedParams{OrgID: f.org, ID: r.ID, Traced: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(BeTrue())
		traced, err = q.DecideRunTraced(ctx, store.DecideRunTracedParams{OrgID: f.org, ID: r.ID, Traced: false})
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(BeTrue(), "a resume claims the run again and keeps the first decision")

		other := newFixture(ctx)
		_, err = q.DecideRunTraced(ctx, store.DecideRunTracedParams{OrgID: other.org, ID: r.ID, Traced: false})
		Expect(err).To(MatchError(pgx.ErrNoRows), "another org cannot decide this run")
	})

	It("numbers a run's turns without gaps and keeps each turn's trace ID", func(ctx SpecContext) {
		f := newFixture(ctx)
		r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		id1, id2 := []byte("0123456789abcdef"), []byte("fedcba9876543210")

		t1, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: id1})
		Expect(err).NotTo(HaveOccurred())
		Expect(t1.Seq).To(Equal(int32(1)))
		t2, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: id2})
		Expect(err).NotTo(HaveOccurred())
		Expect(t2.Seq).To(Equal(int32(2)))

		ended, err := q.EndRunTurn(ctx, store.EndRunTurnParams{OrgID: f.org, ID: t1.ID, EndReason: pgText("park:approval")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ended.EndedAt.Valid).To(BeTrue())
		_, err = q.EndRunTurn(ctx, store.EndRunTurnParams{OrgID: f.org, ID: t1.ID, EndReason: pgText("again")})
		Expect(err).To(MatchError(pgx.ErrNoRows), "a turn ends once")

		list, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: f.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(2))
		Expect(list[1].TraceID).To(Equal(id2))
		byTrace, err := q.GetRunTurnByTrace(ctx, store.GetRunTurnByTraceParams{OrgID: f.org, TraceID: id1})
		Expect(err).NotTo(HaveOccurred())
		Expect(byTrace.ID).To(Equal(t1.ID))

		By("a trace ID is unique, 16 bytes and not zero")
		_, err = q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: id1})
		Expect(err).To(HaveOccurred())
		var pg *pgconn.PgError
		Expect(err).To(BeAssignableToTypeOf(pg))
		_, err = q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: make([]byte, 16)})
		Expect(err).To(HaveOccurred())
		_, err = q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: []byte("short")})
		Expect(err).To(HaveOccurred())

		By("another org's run has no turns of this one")
		other := newFixture(ctx)
		_, err = q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: other.org, RunID: r.ID, TraceID: []byte("ABCDEFGHIJKLMNOP")})
		Expect(err).To(MatchError(pgx.ErrNoRows), "the run is not in that org")
		none, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: other.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(none).To(BeEmpty())
	})

	It("gives concurrent writers distinct, gap-free turn numbers", func(ctx SpecContext) {
		f := newFixture(ctx)
		r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		const writers = 8
		seqs := make(chan int32, writers)
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := make([]byte, 16)
				id[0], id[1] = byte(i+1), 0xAB
				t, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: f.org, RunID: r.ID, TraceID: id})
				errs <- err
				seqs <- t.Seq
			}()
		}
		wg.Wait()
		close(seqs)
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		var got []int
		for sq := range seqs {
			got = append(got, int(sq))
		}
		sort.Ints(got)
		Expect(got).To(Equal([]int{1, 2, 3, 4, 5, 6, 7, 8}))
	})

	It("keeps a platform ceiling row, and org settings cannot send the platform more than their own level", func(ctx SpecContext) {
		ceiling, err := q.GetPlatformTraceSettings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ceiling.ContentCeiling).To(Equal(store.TraceContentLevelFull), "no restriction until a platform admin sets one")

		f := newFixture(ctx)
		_, err = q.UpsertOrgTraceSettings(ctx, store.UpsertOrgTraceSettingsParams{
			OrgID: f.org, ContentLevel: store.TraceContentLevelNone, SampleRate: numeric("1"), PlatformCopyLevel: store.TraceContentLevelRedacted,
		})
		Expect(err).To(HaveOccurred(), "the platform copy above the org's own level violates the table's check")
	})
})
