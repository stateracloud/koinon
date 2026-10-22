// SPDX-License-Identifier: Apache-2.0

package execution

import "encoding/hex"

// Names of the OpenShell objects Koinon owns, shared by the reconciler (which creates them)
// and koinon-agentd (which execs into them). OpenShell 0.1.5 limits names to 19 characters,
// so a full UUID does not fit. The names use the last 8 bytes of the UUID, which in a UUIDv7
// are random (62 bits), not the leading timestamp: two objects created in the same
// millisecond still get different names.

// WorkspaceName is the OpenShell workspace of an org (one per org, ADR 0002).
func WorkspaceName(orgID [16]byte) string {
	return "o-" + hex.EncodeToString(orgID[8:])
}

// AgentSandboxName is the OpenShell sandbox of an agent.
func AgentSandboxName(agentID [16]byte) string {
	return "a-" + hex.EncodeToString(agentID[8:])
}
