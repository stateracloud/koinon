// SPDX-License-Identifier: Apache-2.0

package orcatalog_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var db *pgxpool.Pool

func TestOrcatalog(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "orcatalog")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	if err := testenv.Docker.Err(); err != nil {
		GinkgoWriter.Println("Docker is not available; Postgres specs are skipped:", err)
		return
	}
	db = pgtest.Start(ctx)
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

// uniq returns a model name no other spec uses: the catalog tables are shared by all specs.
func uniq() string { return fmt.Sprintf("fake/m-%d", seq.Add(1)) }

// freshDB is a database of its own for a spec that reads a singleton row.
func freshDB(ctx context.Context) *pgxpool.Pool { return pgtest.Start(ctx) }
