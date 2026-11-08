// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var priceActor = audit.System("koinon-api:price-refresh")

// batchInstall writes e as the only event of a batch.
func batchInstall(ctx context.Context, p audit.Beginner, e audit.InstallEvent) (audit.Written, error) {
	return audit.InTx(ctx, p, func(_ *store.Queries, b audit.Batch) error {
		b.Install(e)
		return nil
	})
}

func recordInstall(ctx context.Context, p *pgxpool.Pool, action string) store.InstallAuditEvent {
	GinkgoHelper()
	w, err := audit.InTx(ctx, p, func(_ *store.Queries, b audit.Batch) error {
		b.Install(audit.InstallEvent{
			Actor: priceActor, Action: action, TargetKind: "test", TargetID: "t", After: map[string]any{"n": action},
		})
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Install).To(HaveLen(1))
	return w.Install[0]
}

// installOwner runs fn with the append-only triggers of the install chain disabled, as the table
// owner does to rewrite the log, and enables them again.
func installOwner(ctx context.Context, p *pgxpool.Pool, fn func(tx pgx.Tx)) {
	GinkgoHelper()
	err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		for _, stmt := range []string{
			`ALTER TABLE install_audit_events DISABLE TRIGGER install_audit_events_no_update`,
			`ALTER TABLE install_audit_chain_head DISABLE TRIGGER install_audit_chain_head_guard`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		fn(tx)
		for _, stmt := range []string{
			`ALTER TABLE install_audit_events ENABLE TRIGGER install_audit_events_no_update`,
			`ALTER TABLE install_audit_chain_head ENABLE TRIGGER install_audit_chain_head_guard`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
}

// asRole runs sql on a connection of p that has switched to role.
func asRole(ctx context.Context, p *pgxpool.Pool, role, sql string, args ...any) error {
	GinkgoHelper()
	conn, err := p.Acquire(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Release()
	_, err = conn.Exec(ctx, `SET ROLE `+role)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	_, err = conn.Exec(ctx, sql, args...)
	return err
}

const insertSQL = `INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary)
	VALUES ('system', 'koinon-api:test', 'test.direct', 'test', 't', '{}')`

var roleSeq atomic.Int64

var _ = Describe("the install audit chain", func() {
	var db *pgxpool.Pool

	BeforeEach(func(ctx SpecContext) {
		// A database of its own: the install chain is one chain per database.
		db = pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
	})

	// This spec does not test the org order (batch_test.go does): it shows that the install head, taken
	// first, serialises every transaction that writes an install event, whatever the org writes after it.
	It("serialises transactions that write an install event on the install head, so they do not deadlock on the org heads they write after it (ADR 0100 decision 1.7)", func(ctx SpecContext) {
		a, _, _ := pgtest.NewOrg(ctx, db)
		b, _, _ := pgtest.NewOrg(ctx, db)
		orgs := []pgtype.UUID{a, b}
		if orgs[0].String() > orgs[1].String() {
			orgs[0], orgs[1] = orgs[1], orgs[0]
		}
		one := func(ctx context.Context, n int) error {
			_, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
				b.Install(audit.InstallEvent{Actor: priceActor, Action: "test.cause", TargetKind: "test", TargetID: fmt.Sprint(n)})
				for _, org := range orgs {
					b.Org(audit.Event{Org: org, Actor: priceActor, Action: "test.effect", TargetKind: "test", TargetID: fmt.Sprint(n)})
				}
				return nil
			})
			return err
		}
		Expect(one(ctx, 0)).To(Succeed())

		const workers, rounds = 6, 8
		var wg sync.WaitGroup
		errs := make(chan error, workers*rounds)
		for w := range workers {
			wg.Go(func() {
				for r := range rounds {
					errs <- one(ctx, 1+w*rounds+r)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}

		var installs, effects, linked int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'test.cause'`).Scan(&installs)).To(Succeed())
		Expect(db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE summary->>'install_event_id' IN (SELECT id::text FROM install_audit_events))
			FROM audit_events WHERE action = 'test.effect'`).Scan(&effects, &linked)).To(Succeed())
		Expect(installs).To(Equal(1 + workers*rounds))
		Expect(effects).To(Equal(2 * installs))
		Expect(linked).To(Equal(effects), "each org event points back at its install event")
		res, err := audit.VerifyInstall(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.OK()).To(BeTrue())
	})

	It("starts with a head at 0 and 32 zero bytes, and links each event to the previous one", func(ctx SpecContext) {
		var seq int64
		var hash []byte
		Expect(db.QueryRow(ctx, `SELECT seq, hash FROM install_audit_chain_head`).Scan(&seq, &hash)).To(Succeed())
		Expect(seq).To(BeZero())
		Expect(hash).To(Equal(audit.ZeroHash))

		e1 := recordInstall(ctx, db, "test.one")
		e2 := recordInstall(ctx, db, "test.two")
		e3 := recordInstall(ctx, db, "test.three")
		Expect(e1.ChainSeq).To(BeEquivalentTo(1))
		Expect(e1.PrevHash).To(Equal(audit.ZeroHash))
		Expect(e2.ChainSeq).To(BeEquivalentTo(2))
		Expect(e2.PrevHash).To(Equal(e1.Hash))
		Expect(e3.PrevHash).To(Equal(e2.Hash))
		Expect(e1.Hash).To(HaveLen(32))
		Expect(db.QueryRow(ctx, `SELECT seq, hash FROM install_audit_chain_head`).Scan(&seq, &hash)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(3))
		Expect(hash).To(Equal(e3.Hash))
	})

	It("computes the hash in Go as the database does, with the install ID in the org ID's place", func(ctx SpecContext) {
		for i := range 4 {
			recordInstall(ctx, db, fmt.Sprintf("test.n%d", i))
		}
		rows, err := store.New(db).ListInstallAuditChainRows(ctx, store.ListInstallAuditChainRowsParams{PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(4))
		var install string
		Expect(db.QueryRow(ctx, `SELECT install_id::text FROM install_identity`).Scan(&install)).To(Succeed())
		for _, r := range rows {
			Expect(r.ScopeText).To(Equal(install))
			Expect(audit.InstallRowHash(r)).To(Equal(r.Hash), "chain_seq %d", r.ChainSeq)
		}
		By("a different tag or scope gives a different hash: an exported chain is not another install's")
		r := rows[0]
		r.ScopeText = "00000000-0000-0000-0000-000000000000"
		Expect(audit.InstallRowHash(r)).NotTo(Equal(rows[0].Hash))
	})

	It("takes chain_seq, prev_hash and hash from the trigger, whatever the writer supplies", func(ctx SpecContext) {
		first := recordInstall(ctx, db, "test.first")
		var seq int64
		var prev, hash []byte
		Expect(db.QueryRow(ctx, `INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary, chain_seq, prev_hash, hash)
			VALUES ('system', 'koinon-api:test', 'test.forged', 'test', 't', '{}', 99, decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'))
			RETURNING chain_seq, prev_hash, hash`).Scan(&seq, &prev, &hash)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(2))
		Expect(prev).To(Equal(first.Hash))
		Expect(bytes.Equal(hash, bytes.Repeat([]byte{0x22}, 32))).To(BeFalse())
		res, err := audit.VerifyInstall(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.OK()).To(BeTrue())
	})

	It("keeps the chain whole when many transactions write at once", func(ctx SpecContext) {
		const n = 25
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Go(func() {
				_, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
					b.Install(audit.InstallEvent{Actor: priceActor, Action: fmt.Sprintf("test.c%d", i), TargetKind: "test"})
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
		res, err := audit.VerifyInstall(ctx, store.New(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.OK()).To(BeTrue(), "%+v", res.Break)
		Expect(res.Rows).To(BeEquivalentTo(n))
	})

	It("writes the build, a reason and the before/after into the summary, and refuses a secret and an unnamed actor", func(ctx SpecContext) {
		w, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
			b.Install(audit.InstallEvent{
				Actor: audit.CLI("prices accept"), Action: "test.summary", TargetKind: "test", TargetID: "t", Reason: "because", After: map[string]any{"a": 1},
			})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		row := w.Install[0]
		var s string
		Expect(db.QueryRow(ctx, `SELECT summary::text FROM install_audit_events WHERE id = $1`, row.ID).Scan(&s)).To(Succeed())
		Expect(s).To(And(ContainSubstring(`"reason": "because"`), ContainSubstring(`"koinon_version": "`+project.Version+`"`), ContainSubstring(`"after": {"a": 1}`)))

		_, err = batchInstall(ctx, db, audit.InstallEvent{Actor: priceActor, Action: "test.secret", TargetKind: "t", After: map[string]any{"password": "x"}})
		Expect(err).To(MatchError(audit.ErrSecretInSummary))
		_, err = batchInstall(ctx, db, audit.InstallEvent{Actor: audit.Actor{Kind: audit.KindSystem}, Action: "test.unnamed", TargetKind: "t"})
		Expect(err).To(HaveOccurred())
		Expect(func() { audit.System("") }).To(Panic())
		By("the database refuses an empty actor too")
		_, err = db.Exec(ctx, `INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary) VALUES ('system', '', 'x', 'x', 'x', '{}')`)
		Expect(err).To(MatchError(ContainSubstring("actor_id")))
	})

	It("puts an org event's pointer back to its install event into the org's summary", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, db)
		inst := recordInstall(ctx, db, "test.cause")
		w, err := audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: org, Actor: priceActor, Action: "test.effect", TargetKind: "t", InstallEventID: inst.ID.String()})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		row := w.Org[0]
		Expect(string(row.Summary)).To(ContainSubstring(`"install_event_id": "` + inst.ID.String() + `"`))
		Expect(string(row.Summary)).To(ContainSubstring(`"koinon_version"`))
	})

	Describe("the guards", func() {
		BeforeEach(func(ctx SpecContext) { recordInstall(ctx, db, "test.keep") })

		It("refuses UPDATE, DELETE and TRUNCATE for the owner too", func(ctx SpecContext) {
			for _, stmt := range []string{
				`UPDATE install_audit_events SET actor_id = 'mallory'`, `DELETE FROM install_audit_events`,
				`TRUNCATE install_audit_events`, `TRUNCATE install_audit_chain_head`,
			} {
				_, err := db.Exec(ctx, stmt)
				Expect(err).To(MatchError(ContainSubstring("append-only")), stmt)
			}
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events`).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		})

		It("lets only the chain trigger change the head", func(ctx SpecContext) {
			_, err := db.Exec(ctx, `UPDATE install_audit_chain_head SET seq = 0`)
			Expect(err).To(MatchError(ContainSubstring("install audit chain trigger")))
			_, err = db.Exec(ctx, `DELETE FROM install_audit_chain_head`)
			Expect(err).To(MatchError(ContainSubstring("install audit chain trigger")))
			_, err = db.Exec(ctx, `INSERT INTO install_audit_chain_head (singleton, seq, hash) VALUES (false, 0, decode(repeat('00', 32), 'hex'))`)
			Expect(err).To(HaveOccurred(), "one head row only")
		})

		It("refuses the writes for a role with no privilege on the table: the group role may only insert", func(ctx SpecContext) {
			// The migration grants INSERT and SELECT to the group role. A member of it, and not of
			// koinon_app, still cannot update, delete or truncate: the triggers and the missing grants.
			role := fmt.Sprintf("install_writer_probe_%d", roleSeq.Add(1))
			_, err := db.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN IN ROLE koinon_install_audit_writer`)
			Expect(err).NotTo(HaveOccurred())
			Expect(asRole(ctx, db, role, insertSQL)).To(Succeed(), "the group role inserts")
			for _, stmt := range []string{`UPDATE install_audit_events SET actor_id = 'x'`, `DELETE FROM install_audit_events`, `TRUNCATE install_audit_events`, `UPDATE install_audit_chain_head SET seq = 0`} {
				Expect(asRole(ctx, db, role, stmt)).NotTo(Succeed(), stmt)
			}
		})
	})

	Describe("the verifier", func() {
		var rows []store.InstallAuditEvent
		BeforeEach(func(ctx SpecContext) {
			rows = nil
			for i := range 5 {
				rows = append(rows, recordInstall(ctx, db, fmt.Sprintf("test.v%d", i)))
			}
		})

		verify := func(ctx context.Context) audit.InstallResult {
			GinkgoHelper()
			res, err := audit.VerifyInstall(ctx, store.New(db))
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		It("passes on a good chain and on an empty one, and reports them", func(ctx SpecContext) {
			r := verify(ctx)
			Expect(r.OK()).To(BeTrue())
			Expect(r.Rows).To(BeEquivalentTo(5))
			var out bytes.Buffer
			Expect(audit.WriteInstallReport(&out, r)).To(BeTrue())
			Expect(out.String()).To(ContainSubstring("ok      install chain: 5 events"))

			empty, err := audit.VerifyInstall(ctx, store.New(pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)))
			Expect(err).NotTo(HaveOccurred())
			Expect(empty.OK()).To(BeTrue())
			Expect(empty.Rows).To(BeZero())
		})

		It("reports the first altered row, not a later one", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				for _, e := range []store.InstallAuditEvent{rows[3], rows[1]} {
					_, err := tx.Exec(ctx, `UPDATE install_audit_events SET actor_id = 'mallory' WHERE id = $1`, e.ID)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			r := verify(ctx)
			Expect(r.OK()).To(BeFalse())
			Expect(r.Break.Seq).To(BeEquivalentTo(2))
			Expect(r.Break.Reason).To(ContainSubstring("hash does not match"))
			var out bytes.Buffer
			Expect(audit.WriteInstallReport(&out, r)).To(BeFalse())
			Expect(out.String()).To(ContainSubstring("BROKEN  install chain: first broken link at chain_seq 2"))
		})

		It("notices a removed row in the middle and a changed prev_hash", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `DELETE FROM install_audit_events WHERE id = $1`, rows[2].ID)
				Expect(err).NotTo(HaveOccurred())
			})
			r := verify(ctx)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(BeEquivalentTo(4))
			Expect(r.Break.Reason).To(ContainSubstring("missing"))
		})

		It("notices a changed prev_hash", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `UPDATE install_audit_events SET prev_hash = decode(repeat('ab', 32), 'hex') WHERE id = $1`, rows[4].ID)
				Expect(err).NotTo(HaveOccurred())
			})
			r := verify(ctx)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(BeEquivalentTo(5))
			Expect(r.Break.Reason).To(ContainSubstring("prev_hash"))
		})

		It("notices a removed tail row: the head no longer matches the last row", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `DELETE FROM install_audit_events WHERE id = $1`, rows[4].ID)
				Expect(err).NotTo(HaveOccurred())
			})
			r := verify(ctx)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Seq).To(BeEquivalentTo(5))
			Expect(r.Break.Reason).To(ContainSubstring("removed"))
		})

		It("notices a removed head", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `DELETE FROM install_audit_chain_head`)
				Expect(err).NotTo(HaveOccurred())
			})
			r := verify(ctx)
			Expect(r.Break).NotTo(BeNil())
			Expect(r.Break.Reason).To(ContainSubstring("head is missing"))
			_, err := batchInstall(ctx, db, audit.InstallEvent{Actor: priceActor, Action: "test.after", TargetKind: "t"})
			Expect(err).To(MatchError(ContainSubstring("head is missing")), "the chain does not restart from zero")
		})

		It("notices a removed head on a chain with no events: the migration seeds it", func(ctx SpecContext) {
			empty := pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
			installOwner(ctx, empty, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `DELETE FROM install_audit_chain_head`)
				Expect(err).NotTo(HaveOccurred())
			})
			r, err := audit.VerifyInstall(ctx, store.New(empty))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Rows).To(BeZero())
			Expect(r.Break).NotTo(BeNil(), "an empty install chain still has its seeded head")
			Expect(r.Break.Seq).To(BeEquivalentTo(1))
			Expect(r.Break.Reason).To(ContainSubstring("head is missing"))
		})

		It("notices a head that was moved back", func(ctx SpecContext) {
			installOwner(ctx, db, func(tx pgx.Tx) {
				_, err := tx.Exec(ctx, `UPDATE install_audit_chain_head SET seq = $1, hash = $2`, rows[3].ChainSeq, rows[3].Hash)
				Expect(err).NotTo(HaveOccurred())
			})
			Expect(verify(ctx).Break).NotTo(BeNil())
		})
	})

	Describe("who may insert, with the roles of the chart's order", func() {
		// The chart creates koinon_app and the group role in postInitApplicationSQL, with default
		// privileges that give koinon_app DML on every table, before the migrate Job runs; the
		// koinon_api managed role is created later and is in both groups, the reconciler's only in
		// koinon_app. This builds that state and migrates a database in it.
		It("gives the group role INSERT, koinon_app SELECT only, and refuses the reconciler's role", func(ctx SpecContext) {
			admin := pgtest.Run(ctx, pgtest.Durable())
			conn, err := pgx.Connect(ctx, admin)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = conn.Close(ctx) })
			n := roleSeq.Add(1)
			api, reconciler := fmt.Sprintf("api_probe_%d", n), fmt.Sprintf("reconciler_probe_%d", n)
			for _, stmt := range []string{
				// koinon_app exists in this container only for this spec; the shared-server rule of pgtest
				// does not apply to Durable.
				`DO $$ BEGIN CREATE ROLE koinon_app NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
				`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO koinon_app`,
				`GRANT USAGE ON SCHEMA public TO koinon_app`,
				fmt.Sprintf(`CREATE ROLE %s NOLOGIN IN ROLE koinon_app`, reconciler),
			} {
				_, err := conn.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred(), stmt)
			}
			By("the chart creates the group role in postInitApplicationSQL, before the first migration")
			// (The test server may have it from building its template; the role is cluster-wide.)
			_, err = conn.Exec(ctx, `DO $$ BEGIN CREATE ROLE koinon_install_audit_writer NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$`)
			Expect(err).NotTo(HaveOccurred())
			pgtest.Migrate(ctx, admin)
			_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s NOLOGIN IN ROLE koinon_app, koinon_install_audit_writer`, api))
			Expect(err).NotTo(HaveOccurred())

			Expect(execAs(ctx, conn, api, insertSQL)).To(Succeed(), "koinon_api inserts")
			Expect(execAs(ctx, conn, api, `SELECT count(*) FROM install_audit_events`)).To(Succeed())
			for _, stmt := range []string{
				insertSQL, `UPDATE install_audit_events SET actor_id = 'x'`, `DELETE FROM install_audit_events`, `TRUNCATE install_audit_events`,
				`UPDATE install_audit_chain_head SET seq = 0`, `TRUNCATE install_audit_chain_head`,
			} {
				err := execAs(ctx, conn, reconciler, stmt)
				if stmt == insertSQL {
					Expect(err).To(MatchError(ContainSubstring("permission denied")), "the reconciler's role cannot insert")
					continue
				}
				Expect(err).To(HaveOccurred(), stmt)
			}
			Expect(execAs(ctx, conn, reconciler, `SELECT count(*) FROM install_audit_events`)).To(Succeed(), "koinon_app reads")
			Expect(execAs(ctx, conn, reconciler, `SELECT count(*) FROM install_audit_chain_head`)).To(Succeed())
			for _, stmt := range []string{`UPDATE install_audit_events SET actor_id = 'x'`, `DELETE FROM install_audit_events`, `TRUNCATE install_audit_events`, `UPDATE install_audit_chain_head SET seq = 0`} {
				err := execAs(ctx, conn, api, stmt)
				Expect(err).To(HaveOccurred(), "not even the writer: "+stmt)
			}
		})

		// A Helm upgrade of a cluster initialised before ADR 0100: the migrate hook runs before the updated
		// Cluster adds the group role, and the migrate role (the initdb owner) has no CREATEROLE. The
		// migration must not abort the upgrade; koinon-api's refused write says why (ErrInstallWriteDenied,
		// which its startup retries on), and the post-upgrade hook (migrate --grants-only) grants the role.
		It("migrates without the group role as a non-CREATEROLE owner, and the grant-only run grants it", func(ctx SpecContext) {
			testenv.Require(testenv.Docker)
			dsn, stop, err := pgtest.StartServer(ctx, pgtest.Durable())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(stop)
			conn, err := pgx.Connect(ctx, dsn)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = conn.Close(ctx) })
			for _, stmt := range []string{
				`CREATE ROLE migrate_probe LOGIN PASSWORD 'koinon_test_not-a-real-password' NOCREATEROLE`,
				`CREATE DATABASE migrate_probe_db OWNER migrate_probe`,
			} {
				_, err := conn.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred(), stmt)
			}
			u, err := url.Parse(dsn)
			Expect(err).NotTo(HaveOccurred())
			u.User = url.UserPassword("migrate_probe", "koinon_test_not-a-real-password")
			u.Path = "/migrate_probe_db"
			Expect(pgtest.MigrateErr(ctx, u.String())).To(Succeed(), "the group role is missing: the upgrade goes on")

			By("the Cluster change arrives: the role exists, a member of it still cannot insert before the grant")
			_, err = conn.Exec(ctx, `CREATE ROLE koinon_install_audit_writer NOLOGIN`)
			Expect(err).NotTo(HaveOccurred())
			member := fmt.Sprintf("upgrade_member_%d", roleSeq.Add(1))
			_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s NOLOGIN IN ROLE koinon_install_audit_writer`, member))
			Expect(err).NotTo(HaveOccurred())
			su, err := url.Parse(dsn)
			Expect(err).NotTo(HaveOccurred())
			su.Path = "/migrate_probe_db"
			mconn, err := pgx.Connect(ctx, su.String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = mconn.Close(ctx) })
			Expect(execAs(ctx, mconn, member, insertSQL)).To(MatchError(ContainSubstring("permission denied")))
			_, err = mconn.Exec(ctx, `SET ROLE `+member)
			Expect(err).NotTo(HaveOccurred())
			_, err = batchInstall(ctx, mconn, audit.InstallEvent{Actor: priceActor, Action: "test.denied", TargetKind: "test"})
			Expect(err).To(MatchError(audit.ErrInstallWriteDenied), "koinon-api can tell a missing grant from other failures")
			_, err = mconn.Exec(ctx, `RESET ROLE`)
			Expect(err).NotTo(HaveOccurred())

			By("the post-upgrade hook (migrate --grants-only, as the migrate role) grants it")
			mdb, err := sql.Open("pgx", u.String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(mdb.Close)
			Expect(migrations.GrantInstallAuditWriter(ctx, mdb)).To(BeTrue())
			Expect(execAs(ctx, mconn, member, insertSQL)).To(Succeed())
		})
	})
})

func execAs(ctx context.Context, conn *pgx.Conn, role, sql string) error {
	GinkgoHelper()
	if _, err := conn.Exec(ctx, `SET ROLE `+role); err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	_, err := conn.Exec(ctx, sql)
	return err
}

var _ = Describe("ListInstall", func() {
	It("pages newest first with the same filters as List, and takes no org", func(ctx SpecContext) {
		db := pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
		for i := range 5 {
			recordInstall(ctx, db, fmt.Sprintf("test.p%d", i))
		}
		recordInstall(ctx, db, "other.thing")
		q := store.New(db)
		page, next, err := audit.ListInstall(ctx, q, audit.Filter{ActionPrefix: "test.", PageSize: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(HaveLen(2))
		Expect(page[0].Action).To(Equal("test.p4"))
		Expect(next).NotTo(BeEmpty())
		page, _, err = audit.ListInstall(ctx, q, audit.Filter{ActionPrefix: "test.", PageSize: 10, PageToken: next})
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(HaveLen(3))
		page, _, err = audit.ListInstall(ctx, q, audit.Filter{ActorKind: audit.KindSystem, ActorID: "koinon-api:price-refresh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(HaveLen(6))

		_, _, err = audit.ListInstall(ctx, q, audit.Filter{Org: pgtype.UUID{Valid: true}})
		Expect(err).To(MatchError(audit.ErrBadFilter))
		_, _, err = audit.ListInstall(ctx, q, audit.Filter{AllOrgs: true})
		Expect(err).To(MatchError(audit.ErrBadFilter))
	})
})

var _ = Describe("koinon_audit_writes_total", func() {
	It("counts writes by chain and result with fixed labels only", func(ctx SpecContext) {
		reg := prometheus.NewRegistry()
		audit.RegisterMetrics(reg)
		db := pgtest.StartTemplate(ctx, pgtest.Durable()).NewDB(ctx)
		org, _, _ := pgtest.NewOrg(ctx, db)
		recordInstall(ctx, db, "test.metric")
		_, err := batchInstall(ctx, db, audit.InstallEvent{Actor: priceActor, Action: "test.secret", TargetKind: "t", After: map[string]any{"password": "x"}})
		Expect(err).To(HaveOccurred())
		_, err = db.Exec(ctx, `ALTER TABLE install_audit_events DISABLE TRIGGER install_audit_events_chain`)
		Expect(err).NotTo(HaveOccurred())
		_, err = batchInstall(ctx, db, audit.InstallEvent{Actor: priceActor, Action: "test.dberr", TargetKind: "t", After: map[string]any{"n": 1}})
		Expect(err).To(HaveOccurred(), "the CHECKs refuse the unchained row")
		_, err = audit.InTx(ctx, db, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: org, Actor: priceActor, Action: "test.org", TargetKind: "t"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		fams, err := reg.Gather()
		Expect(err).NotTo(HaveOccurred())
		Expect(fams).To(HaveLen(1))
		Expect(fams[0].GetName()).To(Equal("koinon_audit_writes_total"))
		series := map[string]float64{}
		for _, m := range fams[0].GetMetric() {
			var labels []string
			key := ""
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName())
				key += l.GetValue() + "/"
			}
			Expect(labels).To(Equal([]string{"chain", "result"}), "fixed labels: no org, user, key or actor ID")
			series[key] = m.GetCounter().GetValue()
		}
		Expect(series).To(HaveKey("install/error/"))
		Expect(series).To(HaveKey("org/error/"), "every series exists at zero")
		Expect(series["install/ok/"]).To(BeNumerically(">=", 1))
		Expect(series["install/error/"]).To(BeNumerically(">=", 1), "a refused insert counts as an error; a scrubbed summary does not reach the database")
		Expect(series["org/ok/"]).To(BeNumerically(">=", 1))
	})
})
