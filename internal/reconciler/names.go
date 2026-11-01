// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/project"
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
	// LabelSupervisorImage on a sandbox holds the OpenShell supervisor image the gateway named when
	// the sandbox was created (ADR 0085 decision 9). A sandbox without it predates the label and is
	// not recreated for a supervisor change.
	LabelSupervisorImage = project.Domain + "/supervisor-image"
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
