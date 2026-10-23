// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestStore(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Store Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("koinon"),
		postgres.BasicWaitStrategies(),
	)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	pool, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)

	db := stdlib.OpenDBFromPool(pool)
	DeferCleanup(db.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())

	q = store.New(pool)
}, NodeTimeout(3*time.Minute))
