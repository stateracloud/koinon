// SPDX-License-Identifier: Apache-2.0

package mcpguard_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	pool *pgxpool.Pool
	q    *store.Queries
	ring *secretstore.Keyring
)

func TestMCPGuard(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MCP Guard Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
	ring = secretstoretest.Keyring()
}, NodeTimeout(3*time.Minute))
