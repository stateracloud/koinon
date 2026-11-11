// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var anchorSpecActor = audit.System("koinon-test:spec")

// Each spec has a database of its own (the install chain is one chain per database) and tampers only
// inside a transaction that is rolled back when the spec ends.
var _ = Describe("install audit anchors", func() {
	var (
		db   *pgxpool.Pool
		orgs []pgtype.UUID
	)

	writeOrg := func(ctx context.Context, org pgtype.UUID, action string) {
		GinkgoHelper()
		_, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: org, Actor: anchorSpecActor, Action: action, TargetKind: "test", TargetID: "t"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	anchor := func(ctx context.Context) audit.AnchorResult {
		GinkgoHelper()
		res, err := audit.Anchor(ctx, db, audit.AnchorOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Written).To(BeTrue())
		return res
	}

	// tamperTx opens a transaction that skips the triggers, as a superuser does to rewrite the log. It is
	// rolled back when the spec ends.
	tamperTx := func(ctx context.Context) pgx.Tx {
		GinkgoHelper()
		tx, err := db.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		Expect(err).NotTo(HaveOccurred())
		return tx
	}

	// ownerTx opens a transaction that, like ownerTx of chain_test.go, disables only the append-only
	// triggers of the org chain, as the table owner does. Unlike tamperTx it leaves the install chain's
	// triggers on, so an anchor can be written in it. It is rolled back when the spec ends, so the
	// tampering never reaches the database; the spec reads and writes through this transaction only.
	ownerTx := func(ctx context.Context) pgx.Tx {
		GinkgoHelper()
		tx, err := db.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		for _, stmt := range []string{
			`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`,
			`ALTER TABLE audit_chain_heads DISABLE TRIGGER audit_chain_heads_guard`,
		} {
			_, err = tx.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
		return tx
	}

	// rewriteOrg rewrites the org's chain from chain_seq from on with a new actor, recomputes every hash
	// and moves the head to match: a self-consistent chain that VerifyOrg accepts.
	rewriteOrg := func(ctx context.Context, tx pgx.Tx, org pgtype.UUID, from int64) {
		GinkgoHelper()
		var prev []byte
		Expect(tx.QueryRow(ctx, `SELECT hash FROM audit_events WHERE org_id = $1 AND chain_seq = $2`, org, from-1).Scan(&prev)).To(Succeed())
		var last int64
		Expect(tx.QueryRow(ctx, `SELECT seq FROM audit_chain_heads WHERE org_id = $1`, org).Scan(&last)).To(Succeed())
		for seq := from; seq <= last; seq++ {
			// audit-lint:allow a tamper test: rewrites an org chain past the triggers so the anchors have a break to find
			_, err := tx.Exec(ctx, `UPDATE audit_events SET actor_id = 'koinon-test:mallory', prev_hash = $3 WHERE org_id = $1 AND chain_seq = $2`, org, seq, prev)
			Expect(err).NotTo(HaveOccurred())
			// audit-lint:allow a tamper test: recomputes the rewritten row's hash
			Expect(tx.QueryRow(ctx, `UPDATE audit_events SET hash = audit_event_hash(audit_events) WHERE org_id = $1 AND chain_seq = $2 RETURNING hash`, org, seq).Scan(&prev)).To(Succeed())
		}
		// audit-lint:allow a tamper test: moves the head to the rewritten chain
		_, err := tx.Exec(ctx, `UPDATE audit_chain_heads SET hash = $2 WHERE org_id = $1`, org, prev)
		Expect(err).NotTo(HaveOccurred())
	}

	installExport := func(ctx context.Context, q *store.Queries) []string {
		GinkgoHelper()
		return exportLinesFrom(ctx, q, audit.ExportScope{Install: true}, 1)
	}
	orgExport := func(ctx context.Context, q *store.Queries, org pgtype.UUID) []string {
		GinkgoHelper()
		return exportLinesFrom(ctx, q, audit.ExportScope{Org: org}, 1)
	}
	anchorSet := func(lines []string) *audit.ExportAnchorSet {
		GinkgoHelper()
		res := verifyLines(lines, audit.ExportVerifyOptions{CollectAnchors: true})
		Expect(res.Break).To(BeNil())
		return &audit.ExportAnchorSet{InstallID: res.Header.InstallID, Anchors: res.StreamAnchors}
	}
	// anchorHeadsLine returns the index of the anchor_heads stream line for the given event id.
	anchorHeadsLine := func(lines []string, eventID string) int {
		GinkgoHelper()
		for i, line := range lines[1:] {
			var m map[string]any
			Expect(json.Unmarshal([]byte(line), &m)).To(Succeed())
			if m["line"] == audit.ExportAnchorLineKind && m["event_id"] == eventID {
				return i + 1
			}
		}
		Fail("no anchor_heads line for event " + eventID)
		return -1
	}

	BeforeEach(func(ctx SpecContext) {
		db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
		orgs = nil
		for range 3 {
			org, _, _ := pgtest.NewOrg(ctx, db)
			orgs = append(orgs, org)
			for i := range 5 {
				writeOrg(ctx, org, fmt.Sprintf("test.n%d", i+1))
			}
		}
	})

	It("writes an audit.anchor event and one side row per org whose root is the Go computation", func(ctx SpecContext) {
		res := anchor(ctx)
		Expect(res.OrgCount).To(Equal(len(orgs)))

		q := store.New(db)
		installID, err := q.GetInstallID(ctx)
		Expect(err).NotTo(HaveOccurred())
		var action, actorKind, actorID, root string
		var count int
		Expect(db.QueryRow(ctx, `SELECT action, actor_kind, actor_id, summary->'after'->>'heads_root', (summary->'after'->>'org_count')::int
			FROM install_audit_events WHERE id::text = $1`, res.EventID).Scan(&action, &actorKind, &actorID, &root, &count)).To(Succeed())
		Expect(action).To(Equal("audit.anchor"))
		Expect(actorKind).To(Equal("system"))
		Expect(actorID).To(Equal("koinon-api:audit-anchor"))
		Expect(count).To(Equal(len(orgs)))

		rows, err := q.ListAuditChainHeads(ctx)
		Expect(err).NotTo(HaveOccurred())
		var heads []audit.AnchorHead
		for _, r := range rows {
			heads = append(heads, audit.AnchorHead{OrgID: r.OrgID.String(), ChainSeq: r.Seq, Hash: r.Hash})
		}
		Expect(heads).To(HaveLen(len(orgs)))
		Expect(root).To(Equal(hex.EncodeToString(audit.HeadsRoot(installID.String(), heads))))
		Expect(res.HeadsRoot).To(Equal(root))

		By("an anchor for each org: the side rows hold the heads, and the root is independent of the input order")
		var stored int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_anchor_heads WHERE anchor_event_id::text = $1 AND chain_seq = 5`, res.EventID).Scan(&stored)).To(Succeed())
		Expect(stored).To(Equal(len(orgs)))
		reversed := []audit.AnchorHead{heads[2], heads[1], heads[0]}
		Expect(audit.HeadsRoot(installID.String(), reversed)).To(Equal(audit.HeadsRoot(installID.String(), heads)))
		Expect(audit.HeadsRoot("another-install", heads)).NotTo(Equal(audit.HeadsRoot(installID.String(), heads)))
	})

	It("refuses UPDATE, DELETE and TRUNCATE of the anchored heads, for the owner too", func(ctx SpecContext) {
		anchor(ctx)
		for _, stmt := range []string{
			// audit-lint:allow a guard test: the statements the triggers must refuse
			`UPDATE install_audit_anchor_heads SET chain_seq = 1`,
			// audit-lint:allow a guard test: the statements the triggers must refuse
			`DELETE FROM install_audit_anchor_heads`,
			// audit-lint:allow a guard test: the statements the triggers must refuse
			`TRUNCATE install_audit_anchor_heads`,
		} {
			_, err := db.Exec(ctx, stmt)
			Expect(err).To(MatchError(ContainSubstring("append-only")), stmt)
		}
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_anchor_heads`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(len(orgs)))
	})

	It("verifies an install chain with anchors, and an org created after the last anchor", func(ctx SpecContext) {
		anchor(ctx)
		late, _, _ := pgtest.NewOrg(ctx, db)
		writeOrg(ctx, late, "test.late")
		writeOrg(ctx, orgs[0], "test.more")

		res, err := audit.VerifyInstall(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.OK()).To(BeTrue(), fmt.Sprint(res.AnchorBreaks))
		Expect(res.Anchors).To(BeEquivalentTo(1))
		var out bytes.Buffer
		Expect(audit.WriteInstallReport(&out, res)).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("1 anchors checked against the org chains"))

		By("and the chains grown since the anchor still verify against the second")
		anchor(ctx)
		res, err = audit.VerifyInstall(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.OK()).To(BeTrue())
		Expect(res.Anchors).To(BeEquivalentTo(2))
	})

	Describe("an org chain rewritten behind an anchor", func() {
		It("is found through the anchor by the database verifier and the file verifier at the same chain_seq, naming the org and anchor", func(ctx SpecContext) {
			a := anchor(ctx)
			writeOrg(ctx, orgs[1], "test.after") // the head moves past the anchor
			tx := tamperTx(ctx)
			q := store.New(tx)
			instLines := installExport(ctx, q)
			rewriteOrg(ctx, tx, orgs[1], 3)

			By("the org's own chain still holds: the rewrite is self-consistent and the head matches")
			org, err := audit.VerifyOrg(ctx, q, orgs[1])
			Expect(err).NotTo(HaveOccurred())
			Expect(org.OK()).To(BeTrue(), "this is what an anchor is for")

			By("the database verifier fails at the anchored chain_seq, naming the org and the anchor")
			res, err := audit.VerifyInstall(ctx, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Break).To(BeNil(), "the install chain itself is intact")
			Expect(res.OK()).To(BeFalse())
			Expect(res.AnchorBreaks).To(HaveLen(1))
			brk := res.AnchorBreaks[0]
			Expect(brk.Org).To(Equal(orgs[1].String()))
			Expect(brk.AnchorID).To(Equal(a.EventID))
			Expect(brk.AnchorSeq).To(Equal(a.ChainSeq))
			Expect(brk.Seq).To(BeEquivalentTo(5))
			var out bytes.Buffer
			Expect(audit.WriteInstallReport(&out, res)).To(BeFalse())
			Expect(out.String()).To(And(ContainSubstring("BROKEN  org "+orgs[1].String()), ContainSubstring(a.EventID), ContainSubstring("rewritten behind the anchor")))

			By("the other orgs are not reported")
			Expect(res.AnchorBreaks).To(HaveLen(1))

			By("the per-org check (verify --org) fails for the rewritten org and passes for another")
			checked, obrks, err := audit.VerifyOrgAnchors(ctx, q, orgs[1].String())
			Expect(err).NotTo(HaveOccurred())
			Expect(checked).To(BeEquivalentTo(1))
			Expect(obrks).To(HaveLen(1))
			Expect(obrks[0].Org).To(Equal(orgs[1].String()))
			Expect(obrks[0].Seq).To(BeEquivalentTo(5))
			_, obrks, err = audit.VerifyOrgAnchors(ctx, q, orgs[0].String())
			Expect(err).NotTo(HaveOccurred())
			Expect(obrks).To(BeEmpty())

			By("the file verifier, given the install export's anchors, fails the org export at the same chain_seq")
			set := anchorSet(instLines)
			fres := verifyLines(orgExport(ctx, q, orgs[1]), audit.ExportVerifyOptions{Anchors: set})
			Expect(fres.Break).NotTo(BeNil())
			Expect(fres.Break.Seq).To(Equal(brk.Seq))
			Expect(fres.Break.Reason).To(ContainSubstring("rewritten behind the anchor"))
			Expect(verifyLines(orgExport(ctx, q, orgs[0]), audit.ExportVerifyOptions{Anchors: set}).Break).To(BeNil())
			Expect(audit.WriteExportReport(&out, fres)).To(BeFalse())

			By("without the anchors the file alone holds")
			Expect(verifyLines(orgExport(ctx, q, orgs[1]), audit.ExportVerifyOptions{}).Break).To(BeNil())
		})

		It("is found when the org's chain is removed, rows and head", func(ctx SpecContext) {
			a := anchor(ctx)
			tx := tamperTx(ctx)
			q := store.New(tx)
			instLines := installExport(ctx, q)
			// audit-lint:allow a tamper test: removes an org's chain, rows and head, past the triggers
			_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1`, orgs[2])
			Expect(err).NotTo(HaveOccurred())
			// audit-lint:allow a tamper test: removes an org's chain head
			_, err = tx.Exec(ctx, `DELETE FROM audit_chain_heads WHERE org_id = $1`, orgs[2])
			Expect(err).NotTo(HaveOccurred())

			res, err := audit.VerifyInstall(ctx, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.AnchorBreaks).To(HaveLen(1))
			Expect(res.AnchorBreaks[0].Org).To(Equal(orgs[2].String()))
			Expect(res.AnchorBreaks[0].AnchorID).To(Equal(a.EventID))
			Expect(res.AnchorBreaks[0].Seq).To(BeEquivalentTo(5))
			Expect(res.AnchorBreaks[0].Reason).To(ContainSubstring("removed"))

			fres := verifyLines(orgExport(ctx, q, orgs[2]), audit.ExportVerifyOptions{Anchors: anchorSet(instLines)})
			Expect(fres.Break).NotTo(BeNil())
			Expect(fres.Break.Seq).To(BeEquivalentTo(5))
		})

		It("is reported at the oldest anchor that fails", func(ctx SpecContext) {
			anchor(ctx)
			writeOrg(ctx, orgs[0], "test.n6")
			second := anchor(ctx)
			tx := tamperTx(ctx)
			rewriteOrg(ctx, tx, orgs[0], 6) // behind the second anchor only
			res, err := audit.VerifyInstall(ctx, store.New(tx))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.AnchorBreaks).To(HaveLen(1))
			Expect(res.AnchorBreaks[0].AnchorID).To(Equal(second.EventID))
			Expect(res.AnchorBreaks[0].Seq).To(BeEquivalentTo(6))
		})
	})

	Describe("a changed, removed or forged anchor", func() {
		It("is found when a side-table row changes, by the database verifier and the export", func(ctx SpecContext) {
			a := anchor(ctx)
			tx := tamperTx(ctx)
			q := store.New(tx)
			// audit-lint:allow a tamper test: changes an anchored head past the triggers
			_, err := tx.Exec(ctx, `UPDATE install_audit_anchor_heads SET hash = decode(repeat('ab', 32), 'hex') WHERE org_id = $1`, orgs[0])
			Expect(err).NotTo(HaveOccurred())

			res, err := audit.VerifyInstall(ctx, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.OK()).To(BeFalse())
			Expect(res.AnchorBreaks[0].AnchorID).To(Equal(a.EventID))
			Expect(res.AnchorBreaks[0].Reason).To(ContainSubstring("heads_root"))

			fres := verifyLines(installExport(ctx, q), audit.ExportVerifyOptions{})
			Expect(fres.Break).NotTo(BeNil())
			Expect(fres.Break.Seq).To(Equal(a.ChainSeq))
			Expect(fres.Break.Reason).To(ContainSubstring("heads_root"))
		})

		It("is found when a side-table row is removed", func(ctx SpecContext) {
			a := anchor(ctx)
			tx := tamperTx(ctx)
			q := store.New(tx)
			// audit-lint:allow a tamper test: removes an anchored head past the triggers
			_, err := tx.Exec(ctx, `DELETE FROM install_audit_anchor_heads WHERE org_id = $1`, orgs[0])
			Expect(err).NotTo(HaveOccurred())

			res, err := audit.VerifyInstall(ctx, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.AnchorBreaks).To(HaveLen(1))
			Expect(res.AnchorBreaks[0].Reason).To(ContainSubstring("commits to 3 org chains"))

			fres := verifyLines(installExport(ctx, q), audit.ExportVerifyOptions{})
			Expect(fres.Break).NotTo(BeNil())
			Expect(fres.Break.Seq).To(Equal(a.ChainSeq))
		})

		It("is found in an export file: a removed or forged stream anchor, a forged head", func(ctx SpecContext) {
			a := anchor(ctx)
			lines := installExport(ctx, store.New(db))
			Expect(verifyLines(lines, audit.ExportVerifyOptions{}).Break).To(BeNil())
			res := verifyLines(lines, audit.ExportVerifyOptions{})
			Expect(res.Anchors).To(Equal(1))
			hi := anchorHeadsLine(lines, a.EventID)

			By("an anchor_heads line removed after the event")
			cut := append(append([]string{}, lines[:hi]...), lines[hi+1:]...)
			r := verifyLines(cut, audit.ExportVerifyOptions{})
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(Equal(a.ChainSeq))
			Expect(r.Break.Reason).To(ContainSubstring("no following anchor_heads line"))

			By("an orphaned anchor_heads line with no preceding audit.anchor event")
			// Drop the audit.anchor event so its heads line follows a non-anchor line.
			orphan := append(append([]string{}, lines[:hi-1]...), lines[hi:]...)
			r = verifyLines(orphan, audit.ExportVerifyOptions{})
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Reason).To(ContainSubstring("no preceding audit.anchor"))

			By("a head with another hash")
			bad := append([]string{}, lines...)
			bad[hi] = editLine(lines[hi], func(m map[string]any) {
				heads := m["heads"].([]any)
				heads[0].(map[string]any)["hash"] = strings.Repeat("ab", 32)
			})
			r = verifyLines(bad, audit.ExportVerifyOptions{})
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(Equal(a.ChainSeq))
			Expect(r.Break.Reason).To(ContainSubstring("heads_root"))

			By("a head removed")
			short := append([]string{}, lines...)
			short[hi] = editLine(lines[hi], func(m map[string]any) {
				m["heads"] = m["heads"].([]any)[1:]
			})
			Expect(verifyLines(short, audit.ExportVerifyOptions{}).Break).NotTo(BeNil())

			By("anchors left in the header are refused")
			_, err := audit.VerifyExport(strings.NewReader(editLine(lines[0], func(m map[string]any) {
				m["anchors"] = []any{map[string]any{"chain_seq": 1}}
			})+"\n"+strings.Join(lines[1:], "\n")+"\n"), audit.ExportVerifyOptions{})
			Expect(err).To(MatchError(audit.ErrExportFormat))
			Expect(err).To(MatchError(ContainSubstring("stream lines")))
		})
	})

	It("round-trips an export with anchors: stream lines carry each anchor's heads, a partial export only its own, and an org export checks against them", func(ctx SpecContext) {
		first := anchor(ctx)
		writeOrg(ctx, orgs[0], "test.n6")
		second := anchor(ctx)
		q := store.New(db)

		lines := installExport(ctx, q)
		var hdr audit.ExportHeader
		Expect(json.Unmarshal([]byte(lines[0]), &hdr)).To(Succeed())
		Expect(hdr.Anchors).To(BeEmpty())
		Expect(hdr.Format).To(Equal(audit.ExportFormat))
		collected := verifyLines(lines, audit.ExportVerifyOptions{CollectAnchors: true})
		Expect(collected.Break).To(BeNil())
		Expect(collected.StreamAnchors).To(HaveLen(2))
		Expect(collected.StreamAnchors[0].ChainSeq).To(Equal(first.ChainSeq))
		Expect(collected.StreamAnchors[1].ChainSeq).To(Equal(second.ChainSeq))
		Expect(collected.StreamAnchors[0].Heads).To(HaveLen(len(orgs)))
		res := verifyLines(lines, audit.ExportVerifyOptions{})
		Expect(res.Break).To(BeNil())
		Expect(res.Anchors).To(Equal(2))
		Expect(res.StreamAnchors).To(BeEmpty(), "without CollectAnchors the verifier discards each stream line")
		var out bytes.Buffer
		Expect(audit.WriteExportReport(&out, res)).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("2 anchors match their events and heads_root"))

		By("a partial export carries only the stream anchors from its from_seq on")
		part := exportLinesFrom(ctx, q, audit.ExportScope{Install: true}, second.ChainSeq)
		var phdr audit.ExportHeader
		Expect(json.Unmarshal([]byte(part[0]), &phdr)).To(Succeed())
		Expect(phdr.Anchors).To(BeEmpty())
		partSet := verifyLines(part, audit.ExportVerifyOptions{CollectAnchors: true})
		Expect(partSet.StreamAnchors).To(HaveLen(1))
		Expect(partSet.StreamAnchors[0].EventID).To(Equal(second.EventID))

		By("an org export holds against the anchors, and a partial one counts what it cannot see")
		set := anchorSet(lines)
		ores := verifyLines(orgExport(ctx, q, orgs[0]), audit.ExportVerifyOptions{Anchors: set})
		Expect(ores.Break).To(BeNil())
		Expect(ores.Anchors).To(Equal(2))
		partial := exportLinesFrom(ctx, q, audit.ExportScope{Org: orgs[0]}, 6)
		pres := verifyLines(partial, audit.ExportVerifyOptions{Anchors: set})
		Expect(pres.Break).To(BeNil())
		Expect(pres.Anchors).To(Equal(1))          // the second anchor names chain_seq 6, which the file holds
		Expect(pres.AnchorsUnchecked).To(Equal(1)) // the first anchor names chain_seq 5 of this org, below the file
		Expect(pres.Partial).To(BeTrue())

		By("anchors of another install are refused, and so are anchors for an install export")
		_, err := audit.VerifyExport(strings.NewReader(strings.Join(orgExport(ctx, q, orgs[0]), "\n")+"\n"),
			audit.ExportVerifyOptions{Anchors: &audit.ExportAnchorSet{InstallID: "another-install"}})
		Expect(err).To(MatchError(ContainSubstring("another install")))
		_, err = audit.VerifyExport(strings.NewReader(strings.Join(lines, "\n")+"\n"), audit.ExportVerifyOptions{Anchors: set})
		Expect(err).To(HaveOccurred())
	})

	Describe("the job", func() {
		countAnchors := func(ctx context.Context) int {
			GinkgoHelper()
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'audit.anchor'`).Scan(&n)).To(Succeed())
			return n
		}
		gauge := func(reg *prometheus.Registry) float64 {
			GinkgoHelper()
			mfs, err := reg.Gather()
			Expect(err).NotTo(HaveOccurred())
			for _, mf := range mfs {
				if mf.GetName() == "koinon_audit_anchor_age_seconds" {
					Expect(mf.GetMetric()[0].GetLabel()).To(BeEmpty(), "no labels (ADR 0023)")
					return mf.GetMetric()[0].GetGauge().GetValue()
				}
			}
			Fail("koinon_audit_anchor_age_seconds is not registered")
			return 0
		}

		It("writes one anchor per interval however many replicas tick together, and sets the age gauge", func(ctx SpecContext) {
			reg := prometheus.NewRegistry()
			audit.RegisterAnchorMetric(reg)
			var wg sync.WaitGroup
			for range 6 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					job := &audit.AnchorJob{DB: db, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
					job.Tick(ctx)
				}()
			}
			wg.Wait()
			Expect(countAnchors(ctx)).To(Equal(1), "six replicas, one tick: one anchor")
			Expect(gauge(reg)).To(BeNumerically("<", 60))

			By("another tick inside the interval writes nothing")
			job := &audit.AnchorJob{DB: db, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			job.Tick(ctx)
			Expect(countAnchors(ctx)).To(Equal(1))

			By("when the interval has passed the next tick anchors again")
			short := &audit.AnchorJob{DB: db, Interval: time.Second, Log: slog.New(slog.DiscardHandler)}
			Eventually(func() int { short.Tick(ctx); return countAnchors(ctx) }, 10*time.Second, 200*time.Millisecond).Should(Equal(2))
			Expect(gauge(reg)).To(BeNumerically("<", 5))
		})

		// inconsistent reads koinon_audit_anchor_inconsistent_total by reason; the counter is shared by the
		// process, so a spec compares before and after.
		inconsistent := func(reg *prometheus.Registry) map[string]float64 {
			GinkgoHelper()
			mfs, err := reg.Gather()
			Expect(err).NotTo(HaveOccurred())
			out := map[string]float64{}
			for _, mf := range mfs {
				if mf.GetName() != "koinon_audit_anchor_inconsistent_total" {
					continue
				}
				for _, m := range mf.GetMetric() {
					Expect(m.GetLabel()).To(HaveLen(1), "one fixed reason label, no org (ADR 0023)")
					Expect(m.GetLabel()[0].GetName()).To(Equal("reason"))
					out[m.GetLabel()[0].GetValue()] = m.GetCounter().GetValue()
				}
				return out
			}
			Fail("koinon_audit_anchor_inconsistent_total is not registered")
			return nil
		}

		It("logs heads_root and the install head of the anchor, and does not count a clean tick as inconsistent", func(ctx SpecContext) {
			reg := prometheus.NewRegistry()
			audit.RegisterAnchorMetric(reg)
			before := inconsistent(reg)
			var logs bytes.Buffer
			job := &audit.AnchorJob{DB: db, Interval: time.Nanosecond, Log: slog.New(slog.NewJSONHandler(&logs, nil))}

			first := anchor(ctx)
			writeOrg(ctx, orgs[0], "test.n6")
			job.Tick(ctx)
			Expect(inconsistent(reg)).To(Equal(before), "a clean tick does not increment the counter")

			var line map[string]any
			Expect(json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line)).To(Succeed(), "one success line, no inconsistency line")
			Expect(line["msg"]).To(Equal(audit.AnchorLogMessage))
			var root, head string
			var seq int64
			Expect(db.QueryRow(ctx, `SELECT summary->'after'->>'heads_root', encode(hash, 'hex'), chain_seq FROM install_audit_events WHERE action = 'audit.anchor' ORDER BY chain_seq DESC LIMIT 1`).Scan(&root, &head, &seq)).To(Succeed())
			Expect(line["heads_root"]).To(Equal(root))
			Expect(line["install_head"]).To(Equal(head))
			Expect(line["chain_seq"]).To(BeEquivalentTo(seq))
			Expect(seq).To(BeNumerically(">", first.ChainSeq))
		})

		Describe("an org chain that disagrees with the previous anchor", func() {
			It("is counted and logged once per tick, and the anchor is still written", func(ctx SpecContext) {
				reg := prometheus.NewRegistry()
				audit.RegisterAnchorMetric(reg)
				before := inconsistent(reg)
				anchor(ctx)
				tx := ownerTx(ctx)
				rewriteOrg(ctx, tx, orgs[1], 3) // behind the anchor, which names chain_seq 5

				var logs bytes.Buffer
				job := &audit.AnchorJob{DB: tx, Interval: time.Nanosecond, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
				job.Tick(ctx)

				after := inconsistent(reg)
				Expect(after[audit.InconsistentHashChanged]).To(Equal(before[audit.InconsistentHashChanged] + 1))
				Expect(after[audit.InconsistentSeqRegressed]).To(Equal(before[audit.InconsistentSeqRegressed]))

				By("the anchor is written anyway, so the chain of anchors goes on")
				var n int
				Expect(tx.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'audit.anchor'`).Scan(&n)).To(Succeed())
				Expect(n).To(Equal(2))

				By("one log line names the org and the reason")
				var warns []string
				for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					if strings.Contains(l, "inconsistent") {
						warns = append(warns, l)
					}
				}
				Expect(warns).To(HaveLen(1))
				Expect(warns[0]).To(And(ContainSubstring(orgs[1].String()), ContainSubstring(audit.InconsistentHashChanged)))
				Expect(logs.String()).To(ContainSubstring(audit.AnchorLogMessage))

				By("verify reports the break at the anchor that was checked")
				res, err := audit.VerifyInstall(ctx, store.New(tx))
				Expect(err).NotTo(HaveOccurred())
				Expect(res.AnchorBreaks).To(HaveLen(1))
				Expect(res.AnchorBreaks[0].Org).To(Equal(orgs[1].String()))
			})

			It("counts a chain head that went down, with the removed row", func(ctx SpecContext) {
				reg := prometheus.NewRegistry()
				audit.RegisterAnchorMetric(reg)
				before := inconsistent(reg)
				anchor(ctx)
				tx := ownerTx(ctx)
				// audit-lint:allow a tamper test: removes the newest row of an org chain, past the triggers
				_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE org_id = $1 AND chain_seq = 5`, orgs[0])
				Expect(err).NotTo(HaveOccurred())
				// audit-lint:allow a tamper test: moves the head back to the row before
				_, err = tx.Exec(ctx, `UPDATE audit_chain_heads SET seq = 4, hash = (SELECT hash FROM audit_events WHERE org_id = $1 AND chain_seq = 4) WHERE org_id = $1`, orgs[0])
				Expect(err).NotTo(HaveOccurred())

				res, err := audit.Anchor(ctx, tx, audit.AnchorOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Written).To(BeTrue())
				Expect(res.Inconsistent).To(ConsistOf(
					audit.AnchorInconsistency{Org: orgs[0].String(), Reason: audit.InconsistentHashChanged, AnchoredSeq: 5},
					audit.AnchorInconsistency{Org: orgs[0].String(), Reason: audit.InconsistentSeqRegressed, AnchoredSeq: 5},
				))
				after := inconsistent(reg)
				Expect(after[audit.InconsistentHashChanged]).To(Equal(before[audit.InconsistentHashChanged] + 1))
				Expect(after[audit.InconsistentSeqRegressed]).To(Equal(before[audit.InconsistentSeqRegressed] + 1))
			})
		})

		It("skips while another transaction is anchoring, and waits when asked to", func(ctx SpecContext) {
			holder, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.New(holder).LockAuditAnchor(ctx)).To(Succeed())

			res, err := audit.Anchor(ctx, db, audit.AnchorOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Written).To(BeFalse())
			Expect(res.Skipped).To(Equal("busy"))

			done := make(chan audit.AnchorResult, 1)
			go func() {
				defer GinkgoRecover()
				r, err := audit.Anchor(ctx, db, audit.AnchorOptions{Wait: true})
				Expect(err).NotTo(HaveOccurred())
				done <- r
			}()
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive())
			Expect(holder.Rollback(ctx)).To(Succeed())
			var r audit.AnchorResult
			Eventually(done, 10*time.Second).Should(Receive(&r))
			Expect(r.Written).To(BeTrue())
		})

		It("never names a head that is not committed", func(ctx SpecContext) {
			// An org write in flight holds its head row; its new head is not committed and not anchored.
			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
			_, err = audit.WriteInTx(ctx, tx, func(b audit.Batch) {
				b.Org(audit.Event{Org: orgs[0], Actor: anchorSpecActor, Action: "test.uncommitted", TargetKind: "test", TargetID: "t"})
			})
			Expect(err).NotTo(HaveOccurred())

			a := anchor(ctx) // does not wait for the open transaction
			var seq int64
			Expect(db.QueryRow(ctx, `SELECT chain_seq FROM install_audit_anchor_heads WHERE anchor_event_id::text = $1 AND org_id = $2`, a.EventID, orgs[0]).Scan(&seq)).To(Succeed())
			Expect(seq).To(BeEquivalentTo(5), "the committed head, not the in-flight chain_seq 6")

			Expect(tx.Commit(ctx)).To(Succeed())
			res, err := audit.VerifyInstall(ctx, store.New(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.OK()).To(BeTrue())
		})
	})
})
