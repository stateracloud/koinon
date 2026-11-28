// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// unreachableBackend cannot open its connection to the sandbox data path: every Exec fails the way the
// Substrate driver reports a router that refuses the handshake.
type unreachableBackend struct{ agentd.Backend }

func (unreachableBackend) Exec(context.Context, string, string, execution.ExecRequest) (execution.Process, error) {
	return nil, &execution.Error{Op: "Exec", Kind: execution.ErrUnreachable, Err: errors.New("TLS identity check with the atenet router failed: remote error: tls: bad certificate")}
}

var _ = Describe("A sandbox data path that cannot be reached", func() {
	It("fails the run at once with the dial error, not after six takeovers", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.backendOverride = unreachableBackend{Backend: e.backend}
		id := e.enqueue(ctx, e.task(ctx, "unreachable"))
		e.pool("agentd-a")

		Eventually(runState(ctx, id)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, id)
		Expect(got.OutcomeReason.String).To(ContainSubstring("TLS identity check with the atenet router failed: remote error: tls: bad certificate"))
		Expect(got.OutcomeReason.String).NotTo(ContainSubstring("harness connection was lost"))
		Expect(got.Takeovers).To(BeZero(), "no lease takeover: the failure is final on the first worker")
	})
})
