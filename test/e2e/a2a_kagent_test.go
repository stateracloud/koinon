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
//   - a kagent release. Tested by koinon-main: kagent 0.10.6 with an echo agent on the fake model server
//     works for Koinon to kagent (state completed, both A2A 0.3 and 1.0 interfaces on its card).
//     kagent v1.0.1 does NOT start alone: its controller needs Substrate in the cluster (it dials ate-api at
//     api.ate-system.svc:443) and a Postgres with the kagent_owner and kagent_readwrite roles and schemas.
//     Pin the release in the test when the run moves to a cluster with Substrate (ADR 0079: at least 3 days
//     old; hack/dep-age-check.sh origin/main). It is not pinned here;
//   - a kagent agent whose model is the fake model server, so no paid model is called;
//   - a Koinon agent with a direct_message trigger, a remote_agents entry for the kagent agent, and an org
//     API token (fake in tests: koinon_test_not-a-real-token);
//   - the Service DNS name of the kagent agent as the target URL, never a port-forward to 127.0.0.1: the
//     sameOrigin check in internal/a2aout/client.go keeps only card interfaces with the scheme and host of
//     the target, and a kagent card names its Service DNS name, so a port-forward leaves no interface.
//
// The full two-way run is part of the done-when of issue 131. It runs on a cluster with Substrate after
// issue 1170. The kagent to Koinon direction needs a full Koinon install and has not run.
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
