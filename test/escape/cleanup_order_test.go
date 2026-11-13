// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

// The sandbox cleanup waits until the gateway no longer lists the sandbox, so the provider
// cleanup that runs next does not meet a sandbox record that still references it (issue 978).
// Ginkgo runs DeferCleanup callbacks last in, first out: g.provider registers before g.sandbox,
// so the sandbox is removed first.
var _ = Describe("sandbox cleanup wait", func() {
	It("returns once the sandbox is gone", func(ctx SpecContext) {
		calls := 0
		err := waitSandboxGone(ctx, "sb", time.Minute, func(context.Context) error {
			calls++
			return fmt.Errorf("wrapped: %w", execution.ErrNotFound)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(1))
	})

	It("fails when the sandbox stays past the bound", func(ctx SpecContext) {
		err := waitSandboxGone(ctx, "sb", 50*time.Millisecond, func(context.Context) error { return nil })
		Expect(err).To(MatchError(ContainSubstring("still exists")))
	})

	It("reports a lookup error that is not NotFound", func(ctx SpecContext) {
		boom := errors.New("boom")
		err := waitSandboxGone(ctx, "sb", 50*time.Millisecond, func(context.Context) error { return boom })
		Expect(err).To(MatchError(boom))
	})
})
