// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	. "github.com/onsi/ginkgo/v2"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// A2A interop with a kagent agent, both directions (issue 131, moved to 1129 for the kind test; ADR 0123).
//
// The exchange of one task between the Koinon outbound client and the Koinon edge, with
// input-required, runs without a cluster in internal/a2aout (loop_test.go). The state mapping is in
// internal/a2aedge (state_test.go). The specs below need a real kagent agent, so they need kind. They
// skip unless the lab gives them these (not a required CI job):
//
//	KOINON_E2E_A2A_URL    base URL of the Koinon edge, e.g. https://koinon.example/a2a/v1
//	KOINON_E2E_KAGENT_URL base URL of the A2A endpoint of a kagent agent
//
// What a run needs (kind, per the issue):
//   - a kind cluster with Koinon installed and an explicit KUBECONFIG file;
//   - a kagent release pinned to a version at least 3 days old (ADR 0079); it is NOT pinned here, because
//     the lane VM could not read kagent releases or reach registry.k8s.io, quay.io and docker.io;
//   - a kagent agent whose model is the fake model server, so no paid model is called;
//   - a Koinon agent with a direct_message trigger, a remote_agents entry for the kagent agent, and an org
//     API token (fake in tests: koinon_test_not-a-real-token).
var _ = Describe("A2A interop with kagent", Label("a2a", "kagent"), func() {
	BeforeEach(func() {
		testenv.Require(testenv.Vars("kagent", "KOINON_E2E_A2A_URL", "KOINON_E2E_KAGENT_URL"))
	})

	PIt("lets a kagent agent send a task to a Koinon agent", func() {
		Skip("pending, needs kind: send a v0.3 and a v1.0 SendMessage from the kagent agent to the Koinon edge; expect a task in input-required when the Koinon agent asks")
	})

	PIt("lets a Koinon agent send a task to a kagent agent", func() {
		Skip("pending, needs kind: the Koinon agent calls send_task on the remote_agents entry; expect completed with the reply of the fake model")
	})

	PIt("keeps input-required across both systems", func() {
		Skip("pending, needs kind: the kagent agent answers a question of the Koinon agent, and the Koinon agent answers a question of the kagent agent")
	})
})
