// SPDX-License-Identifier: Apache-2.0

package schedule_test

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

func TestSchedule(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Schedule triggers Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	dsn = pool.Config().ConnString()
	Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
})
