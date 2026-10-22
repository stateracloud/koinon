// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/project"
)

// MaxSandboxName is OpenShell 0.1.5's limit on sandbox names (found on the home lab, PR #21).
// Provider names follow the same limit to be safe.
const MaxSandboxName = 19

// Owner labels on everything the reconciler renders (workspaces, provider records, sandboxes,
// key Secrets). Values are IDs, never display names.
var (
	LabelManagedBy = project.Domain + "/managed-by"
	LabelOrg       = project.Domain + "/org-id"
	LabelAgent     = project.Domain + "/agent-id"
	// LabelSpecHash on a sandbox holds the hash of the spec it was created from, so a changed
	// spec (image, providers, filesystem policy) is detected and the sandbox recreated.
	LabelSpecHash = project.Domain + "/spec-hash"
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "koinon-reconciler"
)

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

// uuidString formats a pgtype.UUID in its canonical lower-case form.
func uuidString(u pgtype.UUID) string {
	b := u.Bytes
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

// WorkspaceName is the OpenShell workspace of an org: "kw-" and 16 hex characters. In managed
// mode OpenShell prefixes it to form the namespace name, so it is kept short.
func WorkspaceName(org pgtype.UUID) string {
	return "kw-" + shortHash("workspace", uuidString(org))
}

// Names are the per-agent object names, all derived from (org_id, agent_id).
type Names struct {
	Sandbox  string
	Provider string
	// KeySecret is the Kubernetes Secret in Koinon's namespace that holds the agent's
	// agentgateway key.
	KeySecret string
}

// AgentNames derives an agent's object names. Sandbox and provider names are 19 characters
// ("ka-"/"kp-" and 16 hex characters).
func AgentNames(org, agent pgtype.UUID) Names {
	h := shortHash("agent", uuidString(org), uuidString(agent))
	return Names{
		Sandbox:   "ka-" + h,
		Provider:  "kp-" + h,
		KeySecret: "koinon-agent-key-" + h,
	}
}

func ownerLabels(org, agent pgtype.UUID) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue, LabelOrg: uuidString(org)}
	if agent.Valid {
		l[LabelAgent] = uuidString(agent)
	}
	return l
}
