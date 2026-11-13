// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/installbinding"
)

var _ = Describe("the release step at start (ADR 0110 decisions 1 and 2)", func() {
	It("is its own step: a failing attempt does not open the gate, and the retry loop runs until the context ends", func() {
		// A pool that cannot connect: every attempt fails and the row cannot be read, so the gate stays closed.
		pool, err := pgxpool.New(context.Background(), "postgres://koinon@127.0.0.1:1/koinon?connect_timeout=1")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pool.Close)
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waits := 0
		r := &releaseAtStart{
			log:     quiet,
			binding: installbinding.New(pool, "hash-live", false, quiet),
			after: func(time.Duration) <-chan time.Time {
				waits++
				if waits == 3 {
					cancel()
				}
				c := make(chan time.Time, 1)
				c <- time.Time{}
				return c
			},
		}
		Expect(r.Try(ctx, time.Second)).To(BeFalse(), "the first attempt fails and the caller starts the retry")
		r.Retry(ctx)
		Expect(waits).To(Equal(3), "retried on the backoff until the context ended")
		Expect(r.binding.SessionsHeld()).To(BeTrue(), "the gate fails closed while the row is unreadable")
	})

	It("keeps the session gate closed while the plan fails and the binding row cannot be read", func() {
		// A pool that cannot connect: the row read fails, so the gate must stay closed.
		pool, err := pgxpool.New(context.Background(), "postgres://koinon@127.0.0.1:1/koinon?connect_timeout=1")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pool.Close)
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		b := installbinding.New(pool, "hash-live", false, quiet)
		r := &releaseAtStart{
			log:     quiet,
			binding: b,
			plan: func(context.Context) ([]installbinding.Removal, error) {
				return nil, errors.New("list declarative Secrets: forbidden")
			},
		}
		Expect(r.Try(context.Background(), time.Second)).To(BeFalse())
		Expect(b.SessionsHeld()).To(BeTrue(), "sessions and sign-ins stay refused: the release may be pending")
	})
})
