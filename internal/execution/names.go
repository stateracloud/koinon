// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/stateracloud/koinon/internal/project"
)

// LabelInstall is the label (and, on a provider profile, the annotation) that records which
// install made an OpenShell object. Its value is [OwnerID]. An object without it was made before
// the label existed and counts as this install's (issue 461).
var LabelInstall = project.Domain + "/install"

// OwnerID is the value of [LabelInstall]: 16 hex characters of a hash of the install mark. The mark
// is a random value the chart's ensure-secrets hook Job writes once into a Secret of the install's
// namespace (koinon-install-mark). It is not in Postgres, so it does not travel with a backup:
// a database restored next to the live install, in the same namespace name on another cluster too,
// gets its own mark and so its own owner, and cannot touch the live install's objects. The Secret
// outlives an upgrade and a helm uninstall; an install that loses it (the namespace is deleted)
// no longer owns its objects, and an operator takes them back by creating the Secret again with the
// old token or by deleting the install's sandboxes and providers on the gateway so the reconciler
// makes them again. See the backup and restore guide.
func OwnerID(mark string) string {
	return shortHash("owner", mark)
}

// Names of the OpenShell objects Koinon owns. The reconciler derives them when it first
// renders an agent and records them in agent_sandboxes, which koinon-agentd reads; these
// functions are the one place that derives them. Names come from IDs, never display names.

// MaxSandboxName is OpenShell's limit on sandbox names (MAX_ROUTABLE_NAME_LEN, 19 in 0.1.5 and 0.1.7;
// found on a real cluster, PR old issue 21).
// Provider names follow the same limit to be safe.
const MaxSandboxName = 19

// WorkspacePrefix starts every name [WorkspaceName] returns.
const WorkspacePrefix = "kw-"

// WorkspaceName is the OpenShell workspace of an org (one per org, ADR 0002): "kw-" and 16 hex
// characters. In managed mode OpenShell prefixes it to form the namespace name, so it is kept
// short.
func WorkspaceName(org [16]byte) string {
	return WorkspacePrefix + shortHash("workspace", UUIDString(org))
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
