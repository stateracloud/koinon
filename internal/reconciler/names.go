// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"encoding/hex"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/project"
)

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

func ownerLabels(org, agent pgtype.UUID) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue, LabelOrg: uuidString(org)}
	if agent.Valid {
		l[LabelAgent] = uuidString(agent)
	}
	return l
}
