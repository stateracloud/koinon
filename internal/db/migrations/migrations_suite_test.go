// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	db *sql.DB
	// dsn reaches the suite database; tests that migrate step by step create databases beside it.
	dsn string
)

func TestMigrations(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Migrations Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	dsn = pgtest.Run(ctx)
	db, err = sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)
	pgtest.Migrate(ctx, dsn)
}, NodeTimeout(3*time.Minute))
