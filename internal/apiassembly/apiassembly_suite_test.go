// SPDX-License-Identifier: Apache-2.0

package apiassembly_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var pool *pgxpool.Pool

func TestAPIAssembly(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "API Assembly Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
})
