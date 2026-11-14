// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

var pool *pgxpool.Pool

func TestDurable(t *testing.T) {
	RegisterFailHandler(Fail)
	// A DBOS workflow starts through Postgres: under -race and -p the 1 s default is too short (issue 660).
	// Eventually returns at the first success; the window only tells a hang from a slow machine.
	SetDefaultEventuallyTimeout(racescale.D(10 * time.Second))
	RunSpecs(t, "Durable Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx, pgtest.Durable())
})
