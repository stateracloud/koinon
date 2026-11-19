// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	. "github.com/onsi/ginkgo/v2"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// A2A interop with a kagent agent, both directions (issue #131). This is a skeleton. It skips unless
// the lab gives it a Koinon A2A edge and a kagent agent. It is not a required CI job.
//
//	KOINON_E2E_A2A_URL    the Koinon external A2A edge (ADR 0011); not built yet
//	KOINON_E2E_KAGENT_URL the A2A endpoint of a kagent agent that runs a pinned release and a fake model
//
// The kagent release pin is not set: the lane VM cannot read kagent releases, so no version was checked
// against the 3-day rule (ADR 0079).
var _ = Describe("A2A interop with kagent", Label("a2a", "kagent"), func() {
	BeforeEach(func() {
		testenv.Require(
			testenv.Var("a2a-edge", "KOINON_E2E_A2A_URL"),
			testenv.Var("kagent", "KOINON_E2E_KAGENT_URL"),
		)
	})

	It("lets a Koinon agent call a kagent agent", func() {
		Fail("not built: needs the Koinon external A2A client (ADR 0011, v1.0 follow-up)")
	})

	It("lets a kagent agent call a Koinon agent", func() {
		Fail("not built: needs the Koinon external A2A edge (ADR 0011, v1.0 follow-up)")
	})

	It("maps a parked run that needs input to input-required", func() {
		Fail("not built: needs the Koinon external A2A edge (ADR 0011, v1.0 follow-up)")
	})
})
