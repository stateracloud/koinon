// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"crypto/sha256"
	"encoding/hex"
)

// Names of the OpenShell objects Koinon owns. The reconciler derives them when it first
// renders an agent and records them in agent_sandboxes, which koinon-agentd reads; these
// functions are the one place that derives them. Names come from IDs, never display names.

// MaxSandboxName is OpenShell 0.1.5's limit on sandbox names (found on a real cluster, PR koinon-private#21).
// Provider names follow the same limit to be safe.
const MaxSandboxName = 19

// WorkspaceName is the OpenShell workspace of an org (one per org, ADR 0002): "kw-" and 16 hex
// characters. In managed mode OpenShell prefixes it to form the namespace name, so it is kept
// short.
func WorkspaceName(org [16]byte) string {
	return "kw-" + shortHash("workspace", UUIDString(org))
}

// AgentNames are the per-agent object names, all derived from (org_id, agent_id).
type AgentNames struct {
	Sandbox  string
	Provider string
	// GitHubProvider holds the agent's read-only GitHub installation token (plan item C2),
	// created only for agents with a repo.read grant.
	GitHubProvider string
	// GitHubProfile is the workspace-scoped provider profile of GitHubProvider. It is per
	// agent because its endpoints name the agent's repositories, and a profile's endpoints
	// are where its credential may be sent.
	GitHubProfile string
	// KeySecret is the Kubernetes Secret in Koinon's namespace that holds the agent's
	// agentgateway key.
	KeySecret string
}

// AgentObjectNames derives an agent's object names. Sandbox and provider names are 19
// characters ("ka-"/"kp-"/"kg-" and 16 hex characters of a hash of org and agent ID).
func AgentObjectNames(org, agent [16]byte) AgentNames {
	h := shortHash("agent", UUIDString(org), UUIDString(agent))
	return AgentNames{
		Sandbox:        "ka-" + h,
		Provider:       "kp-" + h,
		GitHubProvider: "kg-" + h,
		GitHubProfile:  "koinon-github-" + h,
		KeySecret:      "koinon-agent-key-" + h,
	}
}

// AgentSandboxName is the OpenShell sandbox of an agent.
func AgentSandboxName(org, agent [16]byte) string {
	return AgentObjectNames(org, agent).Sandbox
}

// TaskSandboxName is the sandbox of one task of an ephemeral agent (ADR 0065 decision 13c): "kt-"
// and 16 hex characters of a hash of org, agent and task ID. A task always maps to the same
// name, so a second run of the same task finds (or recreates) its sandbox, and two tasks never
// share one.
func TaskSandboxName(org, agent, task [16]byte) string {
	return "kt-" + shortHash("task", UUIDString(org), UUIDString(agent), UUIDString(task))
}

// shortHash is 16 hex characters (64 bits) of SHA-256 over the parts, each terminated by a
// NUL so that ("ab","c") and ("a","bc") differ.
func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// UUIDString formats a UUID in its canonical lower-case form, as object names and labels carry it.
func UUIDString(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:16])
	return string(out[:])
}

// DefaultGatewayID is OpenShell's chart default for gateway_id (openshell.gatewayId).
const DefaultGatewayID = "openshell"

// WorkspaceNamespace is the Kubernetes namespace OpenShell's managed workspace mode gives a
// workspace: "openshell-{gateway_id}-{workspace}" (OpenShell's documented convention; its API does
// not return the namespace). The sandbox observer (ADR 0078) reads pods there.
func WorkspaceNamespace(gatewayID, workspace string) string {
	if gatewayID == "" {
		gatewayID = DefaultGatewayID
	}
	return "openshell-" + gatewayID + "-" + workspace
}
