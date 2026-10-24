// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var pool *pgxpool.Pool

func TestKeysCLI(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Keys CLI Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) { pool = pgtest.Start(ctx) }, NodeTimeout(3*time.Minute))
