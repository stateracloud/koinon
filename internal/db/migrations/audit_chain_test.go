// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"io/fs"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// chainMigrationVersion finds the audit hash chain migration by name, so no test hard-codes its number.
func chainMigrationVersion() int64 {
	GinkgoHelper()
	names, err := fs.Glob(migrations.FS, "*_audit_hash_chain.sql")
	Expect(err).NotTo(HaveOccurred())
	Expect(names).To(HaveLen(1))
	v, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
	Expect(err).NotTo(HaveOccurred())
	return v
}

var _ = Describe("the audit hash chain migration", func() {
	It("blocks TRUNCATE of the audit log for every role, the owner included", func(ctx SpecContext) {
		_, org := newUser(ctx)
		_, err := db.ExecContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', 'koinon-test:spec', 'test.keep', 'test')`, org)
		Expect(err).NotTo(HaveOccurred())
		for _, stmt := range []string{`TRUNCATE audit_events`, `TRUNCATE orgs CASCADE`, `TRUNCATE audit_chain_heads`} {
			_, err = db.ExecContext(ctx, stmt)
			Expect(err).To(MatchError(ContainSubstring("append-only")), stmt)
		}
	})

	It("fills chain_seq, prev_hash and hash itself and ignores what a writer supplies", func(ctx SpecContext) {
		_, org := newUser(ctx)
		var seq int64
		var prev, hash []byte
		Expect(db.QueryRowContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind, chain_seq, prev_hash, hash)
			VALUES ($1, 'system', 'koinon-test:spec', 'test.forged', 'test', 7, decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'))
			RETURNING chain_seq, prev_hash, hash`, org).Scan(&seq, &prev, &hash)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(1))
		Expect(prev).To(Equal(make([]byte, 32)))
		var ok bool
		Expect(db.QueryRowContext(ctx, `SELECT hash = audit_event_hash(e) FROM audit_events e WHERE org_id = $1`, org).Scan(&ok)).To(Succeed())
		Expect(ok).To(BeTrue())
	})

	It("cannot be steered by a temp table, for a role that only inserts events", func(ctx SpecContext) {
		_, org := newUser(ctx)
		err := pgtest.EnsureRole(ctx, db, "chain_probe")
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `GRANT SELECT, INSERT ON audit_events TO chain_probe`)
		Expect(err).NotTo(HaveOccurred())
		tx, err := db.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET LOCAL ROLE chain_probe`)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE audit_chain_heads (org_id uuid, seq bigint, hash bytea)`)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, `INSERT INTO pg_temp.audit_chain_heads VALUES ($1, 41, decode(repeat('ee', 32), 'hex'))`, org)
		Expect(err).NotTo(HaveOccurred())
		var seq int64
		var prev []byte
		Expect(tx.QueryRowContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', 'koinon-test:spec', 'test.shadow', 'test') RETURNING chain_seq, prev_hash`, org).
			Scan(&seq, &prev)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(1), "the real head, not the temp table's")
		Expect(prev).To(Equal(make([]byte, 32)))
		_, err = tx.ExecContext(ctx, `RESET ROLE`)
		Expect(err).NotTo(HaveOccurred())
		var headSeq int64
		Expect(tx.QueryRowContext(ctx, `SELECT seq FROM public.audit_chain_heads WHERE org_id = $1`, org).Scan(&headSeq)).To(Succeed())
		Expect(headSeq).To(BeEquivalentTo(1))
		var ok bool
		Expect(tx.QueryRowContext(ctx, `SELECT hash = public.audit_event_hash(e) FROM audit_events e WHERE org_id = $1`, org).Scan(&ok)).To(Succeed())
		Expect(ok).To(BeTrue())
		var tempSeq int64
		Expect(tx.QueryRowContext(ctx, `SELECT seq FROM pg_temp.audit_chain_heads`).Scan(&tempSeq)).To(Succeed())
		Expect(tempSeq).To(BeEquivalentTo(41), "the temp table was not touched")
	})

	It("gives the app role no write access to the chain heads", func(ctx SpecContext) {
		_, org := newUser(ctx)
		_, err := db.ExecContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', 'koinon-test:spec', 'test.head', 'test')`, org)
		Expect(err).NotTo(HaveOccurred())
		err = pgtest.EnsureRole(ctx, db, "koinon_app_probe")
		Expect(err).NotTo(HaveOccurred())
		// The chart's CNPG bootstrap grants DML by default privileges; the migration revokes the
		// writes from koinon_app, and this runs the same statement for the probe role.
		_, err = db.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON audit_chain_heads TO koinon_app_probe`)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.ExecContext(ctx, `REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON audit_chain_heads FROM koinon_app_probe`)
		Expect(err).NotTo(HaveOccurred())
		for _, stmt := range []string{
			`INSERT INTO audit_chain_heads (org_id, seq, hash) VALUES (gen_random_uuid(), 0, decode(repeat('00', 32), 'hex'))`,
			`UPDATE audit_chain_heads SET seq = 0`,
			`DELETE FROM audit_chain_heads`,
			`TRUNCATE audit_chain_heads`,
		} {
			conn, err := db.Conn(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = conn.ExecContext(ctx, `SET ROLE koinon_app_probe`)
			Expect(err).NotTo(HaveOccurred())
			_, err = conn.ExecContext(ctx, stmt)
			Expect(err).To(MatchError(ContainSubstring("permission denied")), stmt)
			_, _ = conn.ExecContext(ctx, `RESET ROLE`)
			Expect(conn.Close()).To(Succeed())
		}
	})

	It("chains the rows that exist when it runs, per org in id order", func(ctx SpecContext) {
		version := chainMigrationVersion()
		old, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		// (Deleted orgs: the manual-owner rule is checked for live orgs only.)
		By("three rows in one org and two in another, inserted out of id order")
		var a, b string
		Expect(old.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('a', 'A', 'team', now()) RETURNING id`).Scan(&a)).To(Succeed())
		Expect(old.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('b', 'B', 'team', now()) RETURNING id`).Scan(&b)).To(Succeed())
		ins := func(org, id, action string) {
			_, err := old.ExecContext(ctx, `INSERT INTO audit_events (org_id, id, actor_kind, action, target_kind, summary)
				VALUES ($1, $2, 'system', $3, 'test', '{"after": {"n": 1}}')`, org, id, action)
			Expect(err).NotTo(HaveOccurred())
		}
		ins(a, "019a0000-0000-7000-8000-000000000003", "test.a3")
		ins(b, "019a0000-0000-7000-8000-000000000002", "test.b2")
		ins(a, "019a0000-0000-7000-8000-000000000001", "test.a1")
		ins(a, "019a0000-0000-7000-8000-000000000005", "test.a5")
		ins(b, "019a0000-0000-7000-8000-000000000001", "test.b1")

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		rows, err := old.QueryContext(ctx, `SELECT org_id::text, action, chain_seq, prev_hash, hash, hash = audit_event_hash(e)
			FROM audit_events e ORDER BY org_id, chain_seq`)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = rows.Close() }()
		type row struct {
			org, action string
			seq         int64
			prev, hash  []byte
			ok          bool
		}
		var got []row
		for rows.Next() {
			var r row
			Expect(rows.Scan(&r.org, &r.action, &r.seq, &r.prev, &r.hash, &r.ok)).To(Succeed())
			got = append(got, r)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(5))
		perOrg := map[string][]row{}
		for _, r := range got {
			Expect(r.ok).To(BeTrue(), r.action)
			perOrg[r.org] = append(perOrg[r.org], r)
		}
		actions := func(org string) []string {
			var out []string
			for _, r := range perOrg[org] {
				out = append(out, r.action)
			}
			return out
		}
		Expect(actions(a)).To(Equal([]string{"test.a1", "test.a3", "test.a5"}))
		Expect(actions(b)).To(Equal([]string{"test.b1", "test.b2"}))
		for _, org := range []string{a, b} {
			prev := make([]byte, 32)
			for i, r := range perOrg[org] {
				Expect(r.seq).To(BeEquivalentTo(i + 1))
				Expect(r.prev).To(Equal(prev))
				prev = r.hash
			}
			var seq int64
			var head []byte
			Expect(old.QueryRowContext(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE org_id = $1`, org).Scan(&seq, &head)).To(Succeed())
			Expect(seq).To(BeEquivalentTo(len(perOrg[org])))
			Expect(head).To(Equal(prev))
		}

		By("the append-only trigger works again, and new rows continue the chains")
		_, err = old.ExecContext(ctx, `UPDATE audit_events SET action = 'x'`)
		Expect(err).To(MatchError(ContainSubstring("append-only")))
		var seq int64
		var prev []byte
		Expect(old.QueryRowContext(ctx, `INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind) VALUES ($1, 'system', 'koinon-test:spec', 'test.a6', 'test') RETURNING chain_seq, prev_hash`, a).
			Scan(&seq, &prev)).To(Succeed())
		Expect(seq).To(BeEquivalentTo(4))
		Expect(prev).To(Equal(perOrg[a][2].hash))
	})
})
