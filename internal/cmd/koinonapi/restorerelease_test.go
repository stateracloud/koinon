// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
	})
})
