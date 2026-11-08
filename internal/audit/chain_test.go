// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func record(ctx context.Context, org pgtype.UUID, action string) store.AuditEvent {
	GinkgoHelper()
	w, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
		b.Org(audit.Event{
			Org: org, Actor: audit.System("koinon-test:spec"), Action: action, TargetKind: "test", TargetID: "t",
			After: map[string]any{"n": action},
		})
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Org).To(HaveLen(1))
	return w.Org[0]
}

// ownerTx opens a transaction with the append-only trigger of audit_events disabled, as the table
// owner does to rewrite the log. It is rolled back when the spec ends, so a spec that tampers with
// the chain leaves no broken chain in the shared database: other specs verify every org's chain.
// The ALTER TABLE holds audit_events locked until then, so the spec reads and writes the log
// through this transaction only (store.New(tx)); a query on the pool would wait for the lock.
func ownerTx(ctx context.Context) pgx.Tx {
	GinkgoHelper()
	tx, err := pool.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`)
	Expect(err).NotTo(HaveOccurred())
	return tx
}

var _ = Describe("the audit hash chain", func() {
	var org, other pgtype.UUID

	BeforeEach(func(ctx SpecContext) {
		org, _, _ = pgtest.NewOrg(ctx, pool)
		other, _, _ = pgtest.NewOrg(ctx, pool)
	})

	It("links each event to the previous one of its org, and the first to 32 zero bytes", func(ctx SpecContext) {
		a1 := record(ctx, org, "test.one")
		b1 := record(ctx, other, "test.one")
		a2 := record(ctx, org, "test.two")
		a3 := record(ctx, org, "test.three")
		b2 := record(ctx, other, "test.two")

		Expect(a1.ChainSeq).To(BeEquivalentTo(1))
		Expect(a1.PrevHash).To(Equal(audit.ZeroHash))
		Expect(a2.ChainSeq).To(BeEquivalentTo(2))
		Expect(a2.PrevHash).To(Equal(a1.Hash))
		Expect(a3.ChainSeq).To(BeEquivalentTo(3))
		Expect(a3.PrevHash).To(Equal(a2.Hash))
		Expect(b1.PrevHash).To(Equal(audit.ZeroHash), "chains are per org")
		Expect(b2.PrevHash).To(Equal(b1.Hash))
		Expect(a1.Hash).To(HaveLen(32))
		Expect(a1.Hash).NotTo(Equal(b1.Hash))
	})

	It("computes the hash in Go as the database does", func(ctx SpecContext) {
		for i := range 4 {
			record(ctx, org, fmt.Sprintf("test.n%d", i))
		}
		rows, err := store.New(pool).ListAuditChainRows(ctx, store.ListAuditChainRowsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(4))
		for _, r := range rows {
			Expect(audit.RowHash(r)).To(Equal(r.Hash), "chain_seq %d", r.ChainSeq)
		}
	})

	It("takes the hash fields from the trigger, whatever the writer supplies", func(ctx SpecContext) {
		first := record(ctx, org, "test.first")
		var seq int64
		var prev, hash []byte
		Expect(pool.QueryRow(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind, chain_seq, prev_hash, hash)
			VALUES ($1, 'system', 'koinon-test:spec', 'test.forged', 'test', 99, decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'))
			RETURNING chain_seq, prev_hash, hash`, org).Scan(&seq, &prev, &hash)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(2))
		Expect(prev).To(Equal(first.Hash))
		Expect(bytes.Equal(hash, bytes.Repeat([]byte{0x22}, 32))).To(BeFalse())
		res, err := audit.Verify(ctx, store.New(pool), org)
		Expect(err).NotTo(HaveOccurred())
		Expect(res[0].OK()).To(BeTrue())
	})

	It("keeps the chain whole when many transactions write at once", func(ctx SpecContext) {
		const n = 25
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Go(func() {
				_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
					b.Org(audit.Event{
						Org: org, Actor: audit.System("koinon-test:spec"), Action: fmt.Sprintf("test.c%d", i), TargetKind: "test",
					})
					return nil
				})
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		res, err := audit.Verify(ctx, store.New(pool), org)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(HaveLen(1))
		Expect(res[0].OK()).To(BeTrue(), "%+v", res[0].Break)
		Expect(res[0].Rows).To(BeEquivalentTo(n))
	})

	Describe("the verifier", func() {
		var rows []store.AuditEvent
		BeforeEach(func(ctx SpecContext) {
			rows = nil
			for i := range 5 {
				rows = append(rows, record(ctx, org, fmt.Sprintf("test.v%d", i)))
			}
			record(ctx, other, "test.other")
		})

		verify := func(ctx context.Context, q *store.Queries, o pgtype.UUID) audit.OrgResult {
			GinkgoHelper()
			res, err := audit.Verify(ctx, q, o)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(HaveLen(1))
			return res[0]
		}

		It("passes on a good chain, for one org and for all", func(ctx SpecContext) {
			r := verify(ctx, store.New(pool), org)
			Expect(r.OK()).To(BeTrue())
			Expect(r.Rows).To(BeEquivalentTo(5))
			all, err := audit.Verify(ctx, store.New(pool), pgtype.UUID{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(all)).To(BeNumerically(">=", 2))
			var out bytes.Buffer
			Expect(audit.WriteReport(&out, all)).To(BeTrue())
			Expect(out.String()).To(ContainSubstring("ok      org " + org.String() + ": 5 events"))
		})

		It("reports the first altered row, not a later one", func(ctx SpecContext) {
			tx := ownerTx(ctx)
			for _, e := range []store.AuditEvent{rows[3], rows[1]} {
				_, err := tx.Exec(ctx, `UPDATE audit_events SET actor_id = 'mallory' WHERE org_id = $1 AND id = $2`, e.OrgID, e.ID)
				Expect(err).NotTo(HaveOccurred())
			}
			q := store.New(tx)
			r := verify(ctx, q, org)
			Expect(r.OK()).To(BeFalse())
			Expect(r.Break.Seq).To(BeEquivalentTo(2))
			Expect(r.Break.Reason).To(ContainSubstring("hash does not match"))
			Expect(verify(ctx, q, other).OK()).To(BeTrue(), "another org's chain is not affected")

			var out bytes.Buffer
			Expect(audit.WriteReport(&out, []audit.OrgResult{r})).To(BeFalse())
			Expect(out.String()).To(ContainSubstring("BROKEN  org " + org.String() + ": first broken link at chain_seq 2"))
		})

		It("notices a removed row in the middle, a rewritten link and removed tail rows", func(ctx SpecContext) {
			tx := ownerTx(ctx)
			q := store.New(tx)
			_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1 AND id = $2`, rows[2].OrgID, rows[2].ID)
			Expect(err).NotTo(HaveOccurred())
			r := verify(ctx, q, org)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(BeEquivalentTo(4))
			Expect(r.Break.Reason).To(ContainSubstring("missing"))

			By("a tail row removed, with the chain otherwise intact")
			_, err = tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1`, other)
			Expect(err).NotTo(HaveOccurred())
			r = verify(ctx, q, other)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Reason).To(ContainSubstring("removed"))
		})

		It("notices a changed prev_hash", func(ctx SpecContext) {
			tx := ownerTx(ctx)
			_, err := tx.Exec(ctx, `UPDATE audit_events SET prev_hash = decode(repeat('ab', 32), 'hex') WHERE org_id = $1 AND id = $2`, rows[4].OrgID, rows[4].ID)
			Expect(err).NotTo(HaveOccurred())
			r := verify(ctx, store.New(tx), org)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(BeEquivalentTo(5))
			Expect(r.Break.Reason).To(ContainSubstring("prev_hash"))
		})
	})

	It("refuses TRUNCATE, directly and through orgs CASCADE, for every role", func(ctx SpecContext) {
		record(ctx, org, "test.keep")
		for _, stmt := range []string{`TRUNCATE audit_events`, `TRUNCATE orgs CASCADE`, `TRUNCATE audit_chain_heads`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).To(MatchError(ContainSubstring("append-only")), stmt)
		}
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("lets only the chain trigger change a chain head", func(ctx SpecContext) {
		record(ctx, org, "test.head")
		_, err := pool.Exec(ctx, `UPDATE audit_chain_heads SET seq = 0 WHERE org_id = $1`, org)
		Expect(err).To(MatchError(ContainSubstring("audit chain trigger")))
		_, err = pool.Exec(ctx, `DELETE FROM audit_chain_heads WHERE org_id = $1`, org)
		Expect(err).To(MatchError(ContainSubstring("audit chain trigger")))
	})
})

var _ = Describe("WriteReport", func() {
	It("says so when there is nothing to verify", func() {
		var out bytes.Buffer
		Expect(audit.WriteReport(&out, nil)).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("no audit events"))
	})
})
