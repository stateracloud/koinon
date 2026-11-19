// SPDX-License-Identifier: Apache-2.0

package loadtest_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/loadtest"
)

func TestLoadtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "gateway load harness")
}

var _ = Describe("Percentile", func() {
	It("is the nearest rank", func() {
		var s []time.Duration
		for i := 1; i <= 100; i++ {
			s = append(s, time.Duration(i)*time.Millisecond)
		}
		Expect(loadtest.Percentile(s, 50)).To(Equal(50 * time.Millisecond))
		Expect(loadtest.Percentile(s, 95)).To(Equal(95 * time.Millisecond))
		Expect(loadtest.Percentile(s, 99)).To(Equal(99 * time.Millisecond))
		Expect(loadtest.Percentile(s, 100)).To(Equal(100 * time.Millisecond))
		Expect(loadtest.Percentile(nil, 50)).To(BeZero())
	})
})

var _ = Describe("Run", func() {
	It("counts calls and errors, and leaves the warmup out", func(ctx SpecContext) {
		var n atomic.Int64
		res, err := loadtest.Run(ctx, loadtest.Spec{
			Name: "fake", Concurrency: 4, Duration: 300 * time.Millisecond, Warmup: 100 * time.Millisecond,
			NewWorker: func(context.Context, int) (loadtest.Worker, error) {
				return loadtest.Worker{Op: func(context.Context) error {
					time.Sleep(5 * time.Millisecond)
					if n.Add(1)%10 == 0 {
						return errors.New("refused 42")
					}
					return nil
				}}, nil
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Calls).To(BeNumerically(">", 50))
		Expect(res.Errors).To(BeNumerically(">", 0))
		Expect(res.ErrorKinds).To(HaveKey("refused"))
		Expect(res.ErrorRate()).To(BeNumerically("~", 0.1, 0.05))
		Expect(res.P50).To(BeNumerically(">=", 5*time.Millisecond))
		Expect(res.P99).To(BeNumerically(">=", res.P50))
		Expect(res.Throughput).To(BeNumerically(">", 0))
		Expect(res.LastError).To(BeNumerically(">=", res.FirstError))
	})

	It("runs the disturbance once and reports its error", func(ctx SpecContext) {
		var during atomic.Int64
		_, err := loadtest.Run(ctx, loadtest.Spec{
			Name: "fake", Concurrency: 1, Duration: 100 * time.Millisecond,
			NewWorker: func(context.Context, int) (loadtest.Worker, error) {
				return loadtest.Worker{Op: func(context.Context) error { time.Sleep(time.Millisecond); return nil }}, nil
			},
			DuringAfter: 20 * time.Millisecond,
			During:      func(context.Context) error { during.Add(1); return errors.New("restart failed") },
		})
		Expect(err).To(MatchError(ContainSubstring("restart failed")))
		Expect(during.Load()).To(Equal(int64(1)))
	})

	It("refuses a spec without workers", func(ctx SpecContext) {
		_, err := loadtest.Run(ctx, loadtest.Spec{Name: "bad"})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Report", func() {
	It("writes a Markdown row per result", func() {
		var b bytes.Buffer
		Expect(loadtest.Report(&b, []loadtest.Result{{Driver: "d", Scenario: "s", Concurrency: 2, Calls: 9, Errors: 1, Throughput: 3}})).To(Succeed())
		Expect(b.String()).To(ContainSubstring("| d | s | 2 | 9 | 1 | 10.00 | 3 |"))
	})
})
