// SPDX-License-Identifier: Apache-2.0

package declarative_test

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

func TestDeclarative(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Declarative Secrets Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) { pool = pgtest.Start(ctx) }, NodeTimeout(3*time.Minute))
