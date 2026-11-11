// SPDX-License-Identifier: Apache-2.0

package outward_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var pool *pgxpool.Pool

func TestOutward(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Outward Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	pool = pgtest.Start(ctx)
}, NodeTimeout(3*time.Minute))
