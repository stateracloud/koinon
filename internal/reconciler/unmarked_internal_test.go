// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

func TestClassifyUnmarked(t *testing.T) {
	org := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	id := func(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{2, n}, Valid: true} }
	sbState := func(s store.SandboxState) store.NullSandboxState {
		return store.NullSandboxState{SandboxState: s, Valid: true}
	}
	ws := execution.WorkspaceName(org.Bytes)
	orgs := []store.ListOrgWorkspacesForUnmarkedRow{{OrgID: org}}
	made := map[string]string{LabelManagedBy: ManagedByValue}

	type agent struct {
		status store.AgentStatus
		sb     store.NullSandboxState
	}
	cases := []struct {
		name         string
		agent        agent
		kind         string
		pick         func(execution.AgentNames) string
		labels       map[string]string
		want         string
		wantProvider string
	}{
		{"draft stopped sandbox", agent{store.AgentStatusDraft, sbState(store.SandboxStateStopped)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonWaitsForAgent, ReasonWaitsForAgent},
		{"draft error sandbox", agent{store.AgentStatusDraft, sbState(store.SandboxStateError)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonWaitsForAgent, ReasonWaitsForAgent},
		{"draft pending sandbox row", agent{store.AgentStatusDraft, sbState(store.SandboxStatePending)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonDraftAgent, ReasonDraftAgent},
		{"draft deleted sandbox row", agent{store.AgentStatusDraft, sbState(store.SandboxStateDeleted)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonDraftAgent, ReasonDraftAgent},
		{"draft with no sandbox row", agent{store.AgentStatusDraft, store.NullSandboxState{}}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonDraftAgent, ReasonDraftAgent},
		{"archived retained sandbox", agent{store.AgentStatusArchived, sbState(store.SandboxStateRetained)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonWaitsForAgent, ReasonArchivedAgent},
		{"archived deleting sandbox", agent{store.AgentStatusArchived, sbState(store.SandboxStateDeleting)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonArchivedAgent, ReasonArchivedAgent},
		{"active stopped sandbox", agent{store.AgentStatusActive, sbState(store.SandboxStateStopped)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, made, ReasonPendingClaim, ReasonPendingClaim},
		// The managed-by check: an agent's object name without the reconciler's labels is not its.
		{"no labels", agent{store.AgentStatusActive, sbState(store.SandboxStateReady)}, "sandbox", func(n execution.AgentNames) string { return n.Sandbox }, nil, ReasonNotReconcilerMade, ReasonNotReconcilerMade},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := store.ListAgentsForUnmarkedRow{OrgID: org, ID: id(1), Status: tc.agent.status, Flavor: store.AgentFlavorOpencode, SandboxState: tc.agent.sb}
			n := execution.AgentObjectNames(org.Bytes, a.ID.Bytes)
			objs := []execution.UnmarkedObject{
				{Workspace: ws, Kind: tc.kind, Name: tc.pick(n), Labels: tc.labels},
				{Workspace: ws, Kind: "provider", Name: n.Provider, Labels: tc.labels},
			}
			got := classifyUnmarked(objs, orgs, []store.ListAgentsForUnmarkedRow{a}, nil)
			if got[0].Reason != tc.want {
				t.Errorf("%s: sandbox reason = %s, want %s", tc.name, got[0].Reason, tc.want)
			}
			if got[1].Reason != tc.wantProvider {
				t.Errorf("%s: provider reason = %s, want %s", tc.name, got[1].Reason, tc.wantProvider)
			}
		})
	}
}
