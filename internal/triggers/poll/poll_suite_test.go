// SPDX-License-Identifier: Apache-2.0

package poll_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	pool *pgxpool.Pool
	dsn  string
)

func TestPoll(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Code-host pollers Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	dsn = pool.Config().ConnString()
	Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
})
