// SPDX-License-Identifier: Apache-2.0

package leaderterm_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var pool *pgxpool.Pool

func TestLeaderTerm(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Leaderterm Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	_, err := pool.Exec(ctx, "CREATE TABLE leaderterm_spec (v int NOT NULL)")
	Expect(err).NotTo(HaveOccurred())
})
