// SPDX-License-Identifier: Apache-2.0

package reconciler

// The unmarked list (issue 511; the install mark is ADR 0106).
//
// koinon_openshell_unmarked_objects says how many objects in this install's workspaces carry no
// install mark; ListUnmarked says which, and why each is still unmarked, so an operator can tell
// the ones the reconciler will claim from the ones nothing will. It only reads: it never marks,
// changes or deletes anything, and an object that carries another install's mark is not listed at
// all (the backend leaves it out).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// The reasons an object is still unmarked. They are a fixed set (ADR 0023 applies if a metric ever
// carries them).
const (
	// ReasonOrgNotInDB: the object is in a workspace no org of this database owns (left by an earlier
	// install or by an e2e run on the same gateway; an org that is deleted counts too). Nothing
	// claims it.
	ReasonOrgNotInDB = "org-not-in-db"
	// ReasonDraftAgent: the object belongs to a draft agent. The reconciler claims a draft agent's
	// provider and profile (reconcileDraft) when no sandbox attaches the provider; a stopped sandbox of
	// a draft agent keeps its provider attached and is claimed when the agent is active again.
	ReasonDraftAgent = "draft-agent"
	// ReasonArchivedAgent: the object belongs to an archived agent, or is the flavor profile that only
	// archived agents of the org use. The org pass claims such a profile (claimFlavorProfiles), whenever
	// the archived agent was cleaned up; a retained sandbox is claimed when the agent is unarchived.
	ReasonArchivedAgent = "archived-agent"
	// ReasonNotReconcilerMade: the reconciler did not make it, or this database has no record of it
	// (a sandbox without the managed-by label, a profile that is not one the reconciler renders, a
	// name that matches no agent of the org).
	ReasonNotReconcilerMade = "not-reconciler-made"
	// ReasonPendingClaim: the object belongs to an active agent; its next reconcile claims it
	// (a sandbox is recreated, a provider recreated once no sandbox attaches it, a profile imported).
	ReasonPendingClaim = "pending-claim"
)

// UnmarkedReasons are the values of [UnmarkedObject.Reason].
var UnmarkedReasons = []string{ReasonOrgNotInDB, ReasonDraftAgent, ReasonArchivedAgent, ReasonNotReconcilerMade, ReasonPendingClaim}

// UnmarkedObject is one line of the list.
type UnmarkedObject struct {
	Workspace string `json:"workspace"`
	// Kind is sandbox, provider or profile.
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// unmarkedLister is implemented by a backend that lists, from the gateway, the objects of this
// install's workspaces that carry no install mark (the OpenShell backend with an owner).
type unmarkedLister interface {
	ListUnmarked(ctx context.Context) ([]execution.UnmarkedObject, error)
}

// ListUnmarked lists the unmarked objects with their reasons. The gateway side is the backend's
// (read-only), the database side the orgs and agents of q. It fails when the backend cannot list
// (no install mark, a failed list), so it never answers with a partial list.
func ListUnmarked(ctx context.Context, q *store.Queries, backend execution.Backend) ([]UnmarkedObject, error) {
	l, ok := backend.(unmarkedLister)
	if !ok {
		return nil, errors.New("reconciler: the backend cannot list unmarked objects")
	}
	objs, err := l.ListUnmarked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list unmarked objects: %w", err)
	}
	orgs, err := q.ListOrgWorkspacesForUnmarked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list orgs: %w", err)
	}
	agents, err := q.ListAgentsForUnmarked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list agents: %w", err)
	}
	return classifyUnmarked(objs, orgs, agents), nil
}

// orgView is what the database says about one org's objects.
type orgView struct {
	// names maps an agent-bound object name (sandbox, provider, GitHub provider and profile) to the
	// agent's status.
	names map[string]store.AgentStatus
	// agents maps an agent ID to its status, for the task sandboxes, which carry the agent label.
	agents map[string]store.AgentStatus
	// flavors maps a flavor to the statuses of the org's agents of that flavor.
	flavors map[store.AgentFlavor]map[store.AgentStatus]bool
}

// classifyUnmarked gives every unmarked object its reason. The order of the checks is the order of
// the reasons' certainty: a workspace nobody owns is an orphan whatever is in it; inside an org's
// workspace an object the reconciler did not make comes next; the rest follows the agent.
func classifyUnmarked(objs []execution.UnmarkedObject, orgs []store.ListOrgWorkspacesForUnmarkedRow, agents []store.ListAgentsForUnmarkedRow) []UnmarkedObject {
	byWorkspace := map[string]*orgView{} // workspace -> org
	byOrg := map[pgtype.UUID]*orgView{}
	for _, o := range orgs {
		v := &orgView{
			names: map[string]store.AgentStatus{}, agents: map[string]store.AgentStatus{},
			flavors: map[store.AgentFlavor]map[store.AgentStatus]bool{},
		}
		byOrg[o.OrgID] = v
		// The recorded workspace, and the name a first reconcile would give it.
		byWorkspace[execution.WorkspaceName(o.OrgID.Bytes)] = v
		if o.Workspace.Valid {
			byWorkspace[o.Workspace.String] = v
		}
	}
	for _, a := range agents {
		v := byOrg[a.OrgID]
		if v == nil {
			continue
		}
		n := execution.AgentObjectNames(a.OrgID.Bytes, a.ID.Bytes)
		for _, name := range []string{n.Sandbox, n.Provider, n.GitHubProvider, n.GitHubProfile} {
			v.names[name] = a.Status
		}
		v.agents[execution.UUIDString(a.ID.Bytes)] = a.Status
		if v.flavors[a.Flavor] == nil {
			v.flavors[a.Flavor] = map[store.AgentStatus]bool{}
		}
		v.flavors[a.Flavor][a.Status] = true
	}

	out := make([]UnmarkedObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, UnmarkedObject{
			Workspace: o.Workspace, Kind: o.Kind, Name: o.Name,
			Reason: reasonOf(o, byWorkspace[o.Workspace]),
		})
	}
	return out
}

func reasonOf(o execution.UnmarkedObject, v *orgView) string {
	if v == nil {
		return ReasonOrgNotInDB
	}
	if o.Kind == "profile" {
		if flavor, ok := strings.CutPrefix(o.Name, profilePrefix); ok {
			return flavorReason(v.flavors[store.AgentFlavor(flavor)])
		}
		if st, ok := v.names[o.Name]; ok {
			return statusReason(st)
		}
		return ReasonNotReconcilerMade
	}
	if o.Labels[LabelManagedBy] != ManagedByValue {
		return ReasonNotReconcilerMade
	}
	if st, ok := v.names[o.Name]; ok {
		return statusReason(st)
	}
	if st, ok := v.agents[o.Labels[LabelAgent]]; ok && strings.HasPrefix(o.Name, taskSandboxPrefix) {
		return statusReason(st)
	}
	return ReasonNotReconcilerMade
}

// taskSandboxPrefix starts the names execution.TaskSandboxName returns.
const taskSandboxPrefix = "kt-"

func statusReason(st store.AgentStatus) string {
	switch st {
	case store.AgentStatusActive:
		return ReasonPendingClaim
	case store.AgentStatusArchived:
		return ReasonArchivedAgent
	default:
		return ReasonDraftAgent
	}
}

// flavorReason is the reason of a flavor profile, which every agent of the flavor in the workspace
// shares: any active agent re-imports it on its next pass, so it is pending; otherwise a draft
// agent's claim comes first, then an archived agent's. A profile of a flavor no agent of the org
// has was not left by one of this database's agents (a configured flavor's profile with no agent is
// claimed by the org pass, so it leaves the list on the next pass).
func flavorReason(statuses map[store.AgentStatus]bool) string {
	switch {
	case statuses[store.AgentStatusActive]:
		return ReasonPendingClaim
	case statuses[store.AgentStatusDraft]:
		return ReasonDraftAgent
	case statuses[store.AgentStatusArchived]:
		return ReasonArchivedAgent
	default:
		return ReasonNotReconcilerMade
	}
}
