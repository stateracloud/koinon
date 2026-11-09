// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// batchHarness is what the shared batch specs need from a Runner: two orgs (lo sorts before hi by
// ID) and a count of what was committed. The Postgres batch and the Fake each provide one.
type batchHarness struct {
	runner    audit.Runner
	orgs      func(ctx context.Context) (lo, hi pgtype.UUID)
	installs  func(ctx context.Context) int
	orgEvents func(ctx context.Context, org pgtype.UUID) int
}

func orderedOrgs(a, b pgtype.UUID) (lo, hi pgtype.UUID) {
	if bytes.Compare(a.Bytes[:], b.Bytes[:]) > 0 {
		a, b = b, a
	}
	return a, b
}

var batchActor = audit.System("koinon-test:batch")

func orgEvent(org pgtype.UUID, action string) audit.Event {
	return audit.Event{Org: org, Actor: batchActor, Action: action, TargetKind: "test", TargetID: "t"}
}

// itWritesInOrder is the suite both batches pass: the order, the install link, the refusals and the
// rollback. The Postgres-only specs below cover the hash chain and the locks.
func itWritesInOrder(newHarness func(ctx context.Context) batchHarness) {
	var h batchHarness
	var lo, hi pgtype.UUID

	BeforeEach(func(ctx SpecContext) {
		h = newHarness(ctx)
		lo, hi = h.orgs(ctx)
	})

	It("writes the install events first, then the org events by org ID, whatever order they were added in", func(ctx SpecContext) {
		w, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Org(orgEvent(hi, "test.hi-first"))
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: "t"})
			b.Org(orgEvent(lo, "test.lo"))
			b.Org(orgEvent(hi, "test.hi-second"))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Install).To(HaveLen(1))
		Expect(w.Install[0].Action).To(Equal("test.cause"))
		var got []string
		for _, e := range w.Org {
			got = append(got, e.Action)
		}
		Expect(got).To(Equal([]string{"test.lo", "test.hi-first", "test.hi-second"}), "by org ID, and in the order added within an org")
		Expect(h.installs(ctx)).To(Equal(1))
		Expect(h.orgEvents(ctx, lo)).To(Equal(1))
		Expect(h.orgEvents(ctx, hi)).To(Equal(2))
	})

	It("puts the install event's ID in every org event's summary when the batch has one", func(ctx SpecContext) {
		w, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Org(orgEvent(hi, "test.effect"))
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: "1"})
			b.Org(orgEvent(lo, "test.effect"))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Install).To(HaveLen(1))
		Expect(w.Org).To(HaveLen(2))
		for _, e := range w.Org {
			Expect(audit.SummaryOf(e.Summary)).To(HaveKeyWithValue("install_event_id", w.Install[0].ID.String()))
		}
	})

	It("writes several install events in the order added", func(ctx SpecContext) {
		w, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.first", TargetKind: "test", TargetID: "1"})
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.second", TargetKind: "test", TargetID: "2"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Install).To(HaveLen(2))
		Expect(w.Install[0].Action).To(Equal("test.first"))
		Expect(w.Install[1].Action).To(Equal("test.second"))
		Expect(h.installs(ctx)).To(Equal(2))
	})

	It("refuses to guess the install event of an org event when the batch has several, and writes none of it", func(ctx SpecContext) {
		named := orgEvent(lo, "test.named")
		named.InstallEventID = "caller-chose-this"
		_, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.first", TargetKind: "test", TargetID: "1"})
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.second", TargetKind: "test", TargetID: "2"})
			b.Org(named)
			b.Org(orgEvent(hi, "test.unnamed"))
			return nil
		})
		Expect(err).To(MatchError(audit.ErrAmbiguousInstallLink))
		Expect(h.installs(ctx)).To(BeZero())
		Expect(h.orgEvents(ctx, lo)).To(BeZero())
		Expect(h.orgEvents(ctx, hi)).To(BeZero())
	})

	It("links nothing when the batch has no install event, and keeps an ID the caller set", func(ctx SpecContext) {
		set := orgEvent(hi, "test.set")
		set.InstallEventID = "caller-chose-this"
		w, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Org(orgEvent(lo, "test.plain"))
			b.Org(set)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Install).To(BeEmpty())
		Expect(audit.SummaryOf(w.Org[0].Summary)).NotTo(HaveKey("install_event_id"))
		Expect(audit.SummaryOf(w.Org[1].Summary)).To(HaveKeyWithValue("install_event_id", "caller-chose-this"))
	})

	It("writes nothing for an empty batch", func(ctx SpecContext) {
		w, err := h.runner.InTx(ctx, func(*store.Queries, audit.Batch) error { return nil })
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Install).To(BeEmpty())
		Expect(w.Org).To(BeEmpty())
	})

	It("refuses an event with no named actor, and writes none of the batch", func(ctx SpecContext) {
		for name, add := range map[string]func(audit.Batch){
			"an install event with no actor": func(b audit.Batch) { b.Install(audit.InstallEvent{Action: "test.anon"}) },
			"an install event with no ID": func(b audit.Batch) {
				b.Install(audit.InstallEvent{Actor: audit.Actor{Kind: audit.KindSystem}, Action: "test.anon"})
			},
			"an org event with no actor": func(b audit.Batch) { b.Org(audit.Event{Org: hi, Action: "test.anon"}) },
			"an org event with no ID": func(b audit.Batch) {
				b.Org(audit.Event{Org: hi, Actor: audit.Actor{Kind: audit.KindSystem}, Action: "test.anon"})
			},
			"an org event with no org": func(b audit.Batch) { b.Org(audit.Event{Actor: batchActor, Action: "test.noorg"}) },
			"a summary that carries a secret": func(b audit.Batch) {
				b.Org(audit.Event{Org: hi, Actor: batchActor, Action: "t", After: map[string]any{"apiKey": "x"}})
			},
		} {
			_, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
				b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.good", TargetKind: "test", TargetID: "t"})
				b.Org(orgEvent(lo, "test.good"))
				add(b)
				return nil
			})
			Expect(err).To(HaveOccurred(), name)
			Expect(h.installs(ctx)).To(BeZero(), name)
			Expect(h.orgEvents(ctx, lo)).To(BeZero(), name)
			Expect(h.orgEvents(ctx, hi)).To(BeZero(), name)
		}
	})

	It("rolls the batch back when the function fails, and returns its error", func(ctx SpecContext) {
		boom := errors.New("boom")
		_, err := h.runner.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: "t"})
			b.Org(orgEvent(lo, "test.effect"))
			return boom
		})
		Expect(err).To(MatchError(boom))
		Expect(h.installs(ctx)).To(BeZero())
		Expect(h.orgEvents(ctx, lo)).To(BeZero())
	})
}

var _ = Describe("the audit batch on Postgres", func() {
	var db *pgxpool.Pool

	Describe("the shared suite", func() {
		itWritesInOrder(func(ctx context.Context) batchHarness {
			// A database of its own: the install chain is one chain per database.
			db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
			return batchHarness{
				runner: audit.NewRunner(db),
				orgs: func(ctx context.Context) (pgtype.UUID, pgtype.UUID) {
					a, _, _ := pgtest.NewOrg(ctx, db)
					b, _, _ := pgtest.NewOrg(ctx, db)
					return orderedOrgs(a, b)
				},
				installs: func(ctx context.Context) int {
					var n int
					Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events`).Scan(&n)).To(Succeed())
					return n
				},
				orgEvents: func(ctx context.Context, org pgtype.UUID) int {
					var n int
					Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
					return n
				},
			}
		})
	})

	Describe("the chains and the locks", func() {
		BeforeEach(func(ctx SpecContext) {
			db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
		})

		It("keeps both hash chains whole and links each org event to its install event", func(ctx SpecContext) {
			a, _, _ := pgtest.NewOrg(ctx, db)
			b, _, _ := pgtest.NewOrg(ctx, db)
			for i := range 3 {
				_, err := audit.InTx(ctx, db, func(_ *store.Queries, bt audit.Batch) error {
					bt.Org(orgEvent(b, fmt.Sprint("test.b", i)))
					bt.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: fmt.Sprint(i)})
					bt.Org(orgEvent(a, fmt.Sprint("test.a", i)))
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			ires, err := audit.VerifyInstall(ctx, store.New(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(ires.OK()).To(BeTrue())
			Expect(ires.Rows).To(BeEquivalentTo(3))
			for _, org := range []pgtype.UUID{a, b} {
				res, err := audit.Verify(ctx, store.New(db), org)
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(HaveLen(1))
				Expect(res[0].OK()).To(BeTrue())
				Expect(res[0].Rows).To(BeEquivalentTo(3))
			}
			var linked int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE summary->>'install_event_id' IN (SELECT id::text FROM install_audit_events)`).Scan(&linked)).To(Succeed())
			Expect(linked).To(Equal(6))
		})

		It("writes a batch into a transaction the caller opened, after the caller's own lock (WriteInTx)", func(ctx SpecContext) {
			a, _, _ := pgtest.NewOrg(ctx, db)
			tx, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
			_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('test.write-in-tx'))`)
			Expect(err).NotTo(HaveOccurred())
			w, err := audit.WriteInTx(ctx, tx, func(bt audit.Batch) {
				bt.Org(orgEvent(a, "test.org"))
				bt.Install(audit.InstallEvent{Actor: batchActor, Action: "test.install", TargetKind: "test", TargetID: "1"})
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Install).To(HaveLen(1))
			Expect(w.Org).To(HaveLen(1))
			Expect(audit.SummaryOf(w.Org[0].Summary)).To(HaveKeyWithValue("install_event_id", w.Install[0].ID.String()))

			By("nothing is visible until the caller commits")
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'test.install'`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
			Expect(tx.Commit(ctx)).To(Succeed())
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'test.install'`).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		})

		// Without the batch, a caller that writes org A then B while another writes B then A deadlocks on
		// the org heads. The batch sorts, so both finish.
		It("lets transactions that audit orgs A,B and B,A both finish (ADR 0100 decision 1.7)", func(ctx SpecContext) {
			a, _, _ := pgtest.NewOrg(ctx, db)
			b, _, _ := pgtest.NewOrg(ctx, db)
			for _, withInstall := range []bool{false, true} {
				const workers, rounds = 8, 10
				var wg sync.WaitGroup
				errs := make(chan error, workers*rounds)
				for w := range workers {
					first, second := a, b
					if w%2 == 1 {
						first, second = b, a
					}
					wg.Go(func() {
						for r := range rounds {
							_, err := audit.InTx(ctx, db, func(_ *store.Queries, bt audit.Batch) error {
								bt.Org(orgEvent(first, "test.first"))
								if withInstall && r%2 == 0 {
									bt.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: "t"})
								}
								bt.Org(orgEvent(second, "test.second"))
								return nil
							})
							errs <- err
						}
					})
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					Expect(err).NotTo(HaveOccurred())
				}
			}
			for _, org := range []pgtype.UUID{a, b} {
				res, err := audit.Verify(ctx, store.New(db), org)
				Expect(err).NotTo(HaveOccurred())
				Expect(res[0].OK()).To(BeTrue())
				Expect(res[0].Rows).To(BeEquivalentTo(2 * 8 * 10))
			}
			ires, err := audit.VerifyInstall(ctx, store.New(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(ires.OK()).To(BeTrue())
		})
	})

	Describe("a refused install write", func() {
		// A role that may do everything the change needs but insert into the install chain, as koinon-api
		// is before the chart's post-upgrade grant (ADR 0100 decision 1.1a).
		It("rolls the whole transaction back, domain change and org events with it, and reports ErrInstallWriteDenied", func(ctx SpecContext) {
			db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
			role := fmt.Sprintf("batch_probe_%d", roleSeq.Add(1))
			for _, stmt := range []string{
				fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role),
				fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA public TO %s`, role),
				fmt.Sprintf(`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO %s`, role),
				fmt.Sprintf(`REVOKE ALL ON install_audit_events FROM %s`, role),
				fmt.Sprintf(`REVOKE ALL ON install_audit_chain_head, install_audit_anchor_heads FROM %s`, role),
			} {
				_, err := db.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred(), stmt)
			}
			cfg := db.Config().Copy()
			cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
				_, err := c.Exec(ctx, `SET ROLE `+role)
				return err
			}
			limited, err := pgxpool.NewWithConfig(ctx, cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(limited.Close)

			org, _, _ := pgtest.NewOrg(ctx, db)
			slug := pgtest.Uniq("denied")
			_, err = audit.InTx(ctx, limited, func(q *store.Queries, b audit.Batch) error {
				if _, err := q.CreateOrg(ctx, store.CreateOrgParams{Slug: slug, DisplayName: "Denied", Kind: store.OrgKindTeam, IdentityMode: store.IdentityModeHybrid}); err != nil {
					return err
				}
				b.Org(orgEvent(org, "test.effect"))
				b.Install(audit.InstallEvent{Actor: batchActor, Action: "test.cause", TargetKind: "test", TargetID: "t"})
				return nil
			})
			Expect(err).To(MatchError(audit.ErrInstallWriteDenied))

			var orgs, events int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE slug = $1`, slug).Scan(&orgs)).To(Succeed())
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1`, org).Scan(&events)).To(Succeed())
			Expect(orgs).To(BeZero(), "the domain change rolled back")
			Expect(events).To(BeZero(), "no org event was written")
		})
	})
})

var _ = Describe("the audit batch in memory", func() {
	Describe("the shared suite", func() {
		var f *audit.Fake
		itWritesInOrder(func(context.Context) batchHarness {
			f = &audit.Fake{}
			return batchHarness{
				runner: f,
				orgs: func(context.Context) (pgtype.UUID, pgtype.UUID) {
					var a, b pgtype.UUID
					a.Valid, b.Valid = true, true
					a.Bytes[0], b.Bytes[0] = 0x10, 0x20
					return orderedOrgs(b, a)
				},
				installs:  func(context.Context) int { return len(f.InstallEvents()) },
				orgEvents: func(_ context.Context, org pgtype.UUID) int { return len(f.OrgEvents(org)) },
			}
		})
	})

	It("keeps what a committed transaction audited, for the test of a caller to check", func(ctx SpecContext) {
		f := &audit.Fake{}
		var org pgtype.UUID
		org.Valid = true
		_, err := f.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
			b.Org(orgEvent(org, "test.one"))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.OrgEvents(org)).To(HaveLen(1))
		Expect(f.OrgEvents(org)[0].Action).To(Equal("test.one"))
	})
})
