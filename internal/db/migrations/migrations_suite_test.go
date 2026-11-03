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

// db is the suite database, migrated from nothing here (not copied from pgtest's template: this
// suite is where the migrations themselves run). Tests that migrate step by step take an empty
// database of their own with pgtest.Run.
var db *sql.DB

func TestMigrations(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Migrations Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	dsn := pgtest.Run(ctx)
	db, err = sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)
	pgtest.Migrate(ctx, dsn)
}, NodeTimeout(3*time.Minute))
