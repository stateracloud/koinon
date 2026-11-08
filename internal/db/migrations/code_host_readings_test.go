// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the code-host readings migration (#244)", func() {
	It("keeps every stored repository listed after an upgrade and dates the reading by the newest one", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_code_host_scope_readings.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		dsn := pgtest.Run(ctx)
		db, err := sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		var org, withRepos, withoutRepos, otherOrg, otherConn string
		Expect(db.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('readings', 'Readings', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		Expect(db.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('readings-other', 'Other', 'personal', now()) RETURNING id`).Scan(&otherOrg)).To(Succeed())
		insertConn := func(o, name, fingerprint string) string {
			var id string
			Expect(db.QueryRowContext(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name, secret_fingerprint)
				VALUES ($1, 'github', $2, $3, $4) RETURNING id`, o, name, "koinon-conn-"+name, fingerprint).Scan(&id)).To(Succeed())
			return id
		}
		withRepos = insertConn(org, "withrepos", "SHA256:fingerprint-one")
		withoutRepos = insertConn(org, "norepos", "SHA256:fingerprint-two")
		otherConn = insertConn(otherOrg, "otherorg", "SHA256:fingerprint-three")

		base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		newest := base.Add(3 * time.Hour)
		for _, r := range []struct {
			conn, owner, repo, vis string
			at                     time.Time
		}{
			{withRepos, "acme", "alpha", "private", base},
			{withRepos, "acme", "beta", "internal", newest},
			{withRepos, "acme", "gamma", "public", base.Add(time.Hour)},
			{otherConn, "other", "delta", "private", base.Add(10 * time.Hour)},
		} {
			o := org
			if r.conn == otherConn {
				o = otherOrg
			}
			_, err = db.ExecContext(ctx, `INSERT INTO org_code_host_repos (org_id, connection_id, owner, repo, visibility, checked_at)
				VALUES ($1, $2, $3, $4, $5, $6)`, o, r.conn, r.owner, r.repo, r.vis, r.at)
			Expect(err).NotTo(HaveOccurred())
		}

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("ListOrgCodeHostRepos lists the same repositories as before")
		conn, err := pgx.Connect(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = conn.Close(ctx) })
		var orgID, otherOrgID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(otherOrgID.Scan(otherOrg)).To(Succeed())
		q := store.New(conn)
		rows, err := q.ListOrgCodeHostRepos(ctx, orgID)
		Expect(err).NotTo(HaveOccurred())
		var got []string
		for _, r := range rows {
			got = append(got, r.Owner+"/"+r.Repo+":"+string(r.Visibility))
		}
		Expect(got).To(Equal([]string{"acme/alpha:private", "acme/beta:internal", "acme/gamma:public"}))
		other, err := q.ListOrgCodeHostRepos(ctx, otherOrgID)
		Expect(err).NotTo(HaveOccurred())
		Expect(other).To(HaveLen(1))

		By("the reading carries the connection's fingerprint and the newest checked_at")
		var fingerprint string
		var checked time.Time
		Expect(db.QueryRowContext(ctx, `SELECT secret_fingerprint, checked_at FROM org_code_host_readings WHERE org_id = $1 AND connection_id = $2`,
			org, withRepos).Scan(&fingerprint, &checked)).To(Succeed())
		Expect(fingerprint).To(Equal("SHA256:fingerprint-one"))
		Expect(checked.UTC()).To(Equal(newest))

		By("a connection with no stored repositories gets no reading")
		var n int
		Expect(db.QueryRowContext(ctx, `SELECT count(*) FROM org_code_host_readings WHERE org_id = $1 AND connection_id = $2`,
			org, withoutRepos).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
		Expect(db.QueryRowContext(ctx, `SELECT count(*) FROM org_code_host_readings`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
	})
})
