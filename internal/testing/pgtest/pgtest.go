// SPDX-License-Identifier: Apache-2.0

// Package pgtest starts a migrated Postgres for a Ginkgo suite with testcontainers.
package pgtest

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// Image is the minimum supported Postgres version (ADR 0047).
const Image = "postgres:18-alpine"

// Start runs Postgres and applies every migration; call it from a BeforeSuite. The container
// and the pool are removed when the suite ends.
func Start(ctx context.Context) *pgxpool.Pool {
	ctr, err := postgres.Run(ctx, Image, postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	pool, err := pgxpool.New(ctx, dsn)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	ginkgo.DeferCleanup(db.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = provider.Up(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return pool
}

var seq atomic.Int64

// Uniq returns a name unique within the test process.
func Uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// NewOrg creates an org with an owner and returns their IDs and the org's slug.
func NewOrg(ctx context.Context, pool *pgxpool.Pool) (org, user pgtype.UUID, slug string) {
	slug = Uniq("org")
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Org', 'personal') RETURNING id`, slug).Scan(&org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			Uniq("sub"), org).Scan(&user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, org, user)
		return err
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return org, user, slug
}

// TruncateOrgs empties orgs and every table that references it, for tests that start from nothing.
// The audit log refuses TRUNCATE for every role, the owner included (ADR 0023), and TRUNCATE ...
// CASCADE reaches it, so this runs the statement with session_replication_role = replica, which
// skips ordinary triggers. It needs a role that may set that (the testcontainers superuser).
func TruncateOrgs(ctx context.Context, pool *pgxpool.Pool) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `TRUNCATE orgs CASCADE`)
		return err
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}
