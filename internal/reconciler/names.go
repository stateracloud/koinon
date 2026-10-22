// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/execution"
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

func ownerLabels(org, agent pgtype.UUID) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue, LabelOrg: execution.UUIDString(org.Bytes)}
	if agent.Valid {
		l[LabelAgent] = execution.UUIDString(agent.Bytes)
	}
	return l
}
