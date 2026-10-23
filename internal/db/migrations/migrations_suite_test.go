// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// PostgresImage is the minimum supported Postgres version (ADR 0047).
const PostgresImage = "postgres:18-alpine"

var db *sql.DB

func TestMigrations(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Migrations Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase("koinon"),
		postgres.BasicWaitStrategies(),
	)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	db, err = sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
}, NodeTimeout(3*time.Minute))
