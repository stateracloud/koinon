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
	It("is its own step: a failing plan does not settle, and a retry runs until it succeeds", func() {
		calls := 0
		r := &releaseAtStart{
			// A disabled Binding settles nothing and succeeds, which is enough to see the loop end.
			log:     slog.Default(),
			binding: installbinding.New(nil, "", false, nil),
			plan: func(context.Context) ([]installbinding.Removal, error) {
				calls++
				if calls < 3 {
					return nil, errors.New("list declarative Secrets: forbidden")
				}
				return nil, nil
			},
			after: func(time.Duration) <-chan time.Time {
				c := make(chan time.Time, 1)
				c <- time.Time{}
				return c
			},
		}
		Expect(r.Try(context.Background(), time.Second)).To(BeFalse(), "the first attempt fails and the caller starts the retry")
		r.Retry(context.Background())
		Expect(calls).To(Equal(3), "retried until the plan could be read, then released")
		Expect(r.binding.SessionsHeld()).To(BeFalse(), "a disabled binding has nothing pending")
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
