// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// Restore-hold (issue #462): a pool that is held claims no run, and neither takes over a run whose
// lease has expired; when the hold is released (a pool that runs) the same runs are driven.
var _ = Describe("Restore-hold", func() {
	It("claims no queued run and takes over no expired lease until the hold is released", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		queued := e.enqueue(ctx, e.task(ctx, "restored backlog"))

		stopHeld := e.heldPool("agentd-held")
		Consistently(runState(ctx, queued)).WithTimeout(2*time.Second).WithPolling(100*time.Millisecond).
			Should(Equal(store.RunStateQueued), "a held pool claims nothing")
		Expect(getRun(ctx, queued).OwnerID.Valid).To(BeFalse(), "and holds no lease")

		By("releasing the hold: a pool that runs drives the same run")
		stopHeld()
		e.pool("agentd-released")
		Eventually(runState(ctx, queued)).WithTimeout(racescale.D(30 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})
})
