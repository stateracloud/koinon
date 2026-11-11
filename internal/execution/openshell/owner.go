// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/execution"
	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// WithOwner makes the Backend an owner of what it makes (issue 461): it stamps [execution.LabelInstall]
// with id on every workspace, sandbox, provider and provider profile it creates, and it refuses (with
// [execution.ErrForeign]) to adopt, change, start, stop or delete an object whose label holds another
// value. Two installs on one gateway then cannot touch each other's objects even when their org and
// agent IDs, and so their object names, are the same (a restore next to the live install).
//
// An object without the label was made before it existed. check lets it pass (a gateway may hold
// such objects from before the upgrade), but the live install claims them on its first reconcile:
// OpenShell keeps labels immutable, so the reconciler recreates an unlabeled sandbox (see [Backend.Owner]),
// and, once no sandbox attaches it, EnsureProvider deletes and recreates an unlabeled provider when
// [execution.Provider.ClaimLegacy] is set (until then it is taken as this install's), and a profile's owner is part of its spec
// hash so it is re-imported with the annotation. Workspaces are the exception: they hold sandboxes
// and homes and are not recreated, so an old workspace stays unmarked; a new one is stamped and checked. Until the
// live install has recreated them all (koinon_openshell_unmarked_objects is 0 for every kind and koinon_openshell_unmarked_objects_stale is 0), a second
// install on the gateway can still touch its unlabeled objects; the upgrading page of the docs site, https://koinon.stateracloud.com/docs/operate/upgrading/, says so. An empty id turns
// the checks off (a dev setup without an install mark). koinon-reconciler and koinon-agentd both
// set it: agentd execs and uploads into sandboxes, and a restored database carries the live install's
// "ready" rows. It returns b.
func (b *Backend) WithOwner(id string) *Backend {
	b.owner = id
	return b
}

// Owner returns the id WithOwner set ("" when ownership is off). The reconciler treats a sandbox
// whose [execution.LabelInstall] differs from it as spec drift and recreates it with the label.
func (b *Backend) Owner() string { return b.owner }

// unlabeled reports whether an object made before the install label existed: ownership is on and
// the object carries no install label.
func (b *Backend) unlabeled(labels map[string]string) bool {
	return b.owner != "" && labels[execution.LabelInstall] == ""
}

// UnmarkedKinds are the keys of the map [Backend.CountUnmarked] returns.
var UnmarkedKinds = []string{"sandbox", "provider", "profile"}

// CountUnmarked counts, from the gateway and not from what a reconcile pass touched, the sandboxes,
// providers and provider profiles in this install's workspaces that carry no install mark (issue 461).
// It is the length, by kind, of what [Backend.ListUnmarked] returns, so the gauge and the list command
// always agree. Any failed list returns an error and no counts, so a caller never publishes a partial
// number. Without an owner nothing is stamped, and it returns an error.
func (b *Backend) CountUnmarked(ctx context.Context) (map[string]int, error) {
	objs, err := b.ListUnmarked(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, k := range UnmarkedKinds {
		counts[k] = 0
	}
	for _, o := range objs {
		counts[o.Kind]++
	}
	return counts, nil
}

// ListUnmarked lists, from the gateway, the sandboxes, providers and provider profiles in this
// install's workspaces that carry no install mark (issue 511). This install's workspaces are the ones
// named like [execution.WorkspaceName] whose mark is empty or its own; a workspace marked by another
// install is left out, and so are the objects in it. Inside them an object with another install's mark
// is not listed either: it is not this install's to claim. Sandboxes and providers come from one
// all-workspaces list each, filtered to those workspaces by their metadata; profiles are listed per
// workspace (the API has no all-workspaces selector for them) and only the workspace's own custom
// profiles count: the platform and built-in profiles a workspace listing also returns carry no mark and
// are not Koinon's. Every list runs to the last page. Any failed list returns an error and no objects.
// The result is sorted by workspace, kind and name. It only reads. Without an owner it returns an error.
func (b *Backend) ListUnmarked(ctx context.Context) ([]execution.UnmarkedObject, error) {
	const op = "ListUnmarked"
	if b.owner == "" {
		return nil, &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: errors.New("no install mark is set")}
	}
	mine := func(labels map[string]string) bool {
		return labels[execution.LabelInstall] == "" || labels[execution.LabelInstall] == b.owner
	}
	unmarked := func(labels map[string]string) bool { return labels[execution.LabelInstall] == "" }

	workspaces := map[string]bool{}
	token := ""
	for {
		resp, err := b.c.ListWorkspaces(ctx, &openshellv1.ListWorkspacesRequest{PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, wrap(op, err)
		}
		for _, w := range resp.GetWorkspaces() {
			if strings.HasPrefix(w.GetMetadata().GetName(), execution.WorkspacePrefix) && mine(w.GetMetadata().GetLabels()) {
				workspaces[w.GetMetadata().GetName()] = true
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}

	var out []execution.UnmarkedObject
	all := &datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &datamodelv1.AllWorkspaces{}}}
	for {
		resp, err := b.c.ListSandboxes(ctx, &openshellv1.ListSandboxesRequest{WorkspaceScope: all, PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, wrap(op, err)
		}
		for _, s := range resp.GetSandboxes() {
			if workspaces[s.GetMetadata().GetWorkspace()] && unmarked(s.GetMetadata().GetLabels()) {
				out = append(out, execution.UnmarkedObject{
					Workspace: s.GetMetadata().GetWorkspace(), Kind: "sandbox", Name: s.GetMetadata().GetName(), Labels: s.GetMetadata().GetLabels(),
				})
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	for {
		resp, err := b.c.ListProviders(ctx, &openshellv1.ListProvidersRequest{WorkspaceScope: all, PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, wrap(op, err)
		}
		for _, p := range resp.GetProviders() {
			if workspaces[p.GetMetadata().GetWorkspace()] && unmarked(p.GetMetadata().GetLabels()) {
				out = append(out, execution.UnmarkedObject{
					Workspace: p.GetMetadata().GetWorkspace(), Kind: "provider", Name: p.GetMetadata().GetName(), Labels: p.GetMetadata().GetLabels(),
				})
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	for ws := range workspaces {
		for {
			resp, err := b.c.ListProviderProfiles(ctx, &openshellv1.ListProviderProfilesRequest{WorkspaceScope: scope(ws), PageSize: 1000, PageToken: token})
			if err != nil {
				return nil, wrap(op, err)
			}
			for _, p := range resp.GetProfiles() {
				if p.GetScope() == "workspace" && p.GetSource() != "builtin" && unmarked(p.GetAnnotations()) {
					out = append(out, execution.UnmarkedObject{
						Workspace: ws, Kind: "profile", Name: p.GetId(), Labels: p.GetAnnotations(),
					})
				}
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	slices.SortFunc(out, func(a, c execution.UnmarkedObject) int {
		return cmp.Or(strings.Compare(a.Workspace, c.Workspace), strings.Compare(a.Kind, c.Kind), strings.Compare(a.Name, c.Name))
	})
	return out, nil
}

// ClaimProviderProfile marks a workspace profile that exists without an install mark (issue 511): it
// imports the profile again as p, which stamps the mark (the owner is part of the profile's spec
// hash). A missing profile is not created (false, nil), and a profile that carries a mark is left as
// it is: the active agents of its flavor keep it current through [Backend.EnsureProvider]. A profile
// marked by another install fails with [execution.ErrForeign]. It reports whether it claimed one.
func (b *Backend) ClaimProviderProfile(ctx context.Context, workspace string, p execution.ProviderProfile) (bool, error) {
	const op = "ClaimProviderProfile"
	if b.owner == "" {
		return false, nil
	}
	// One read under profileMu serves the check and the write, so the update goes against the
	// version the check looked at and no concurrent ensureProfile slips in between.
	b.profileMu.Lock()
	defer b.profileMu.Unlock()
	got, err := b.c.GetProviderProfile(ctx, &openshellv1.GetProviderProfileRequest{WorkspaceScope: scope(workspace), Id: p.ID})
	if err != nil {
		if errors.Is(wrap(op, err), execution.ErrNotFound) {
			return false, nil
		}
		return false, wrap(op, err)
	}
	labels := got.GetProfile().GetAnnotations()
	if err := b.check(op, "provider profile", p.ID, labels); err != nil {
		return false, err
	}
	if !b.unlabeled(labels) {
		return false, nil
	}
	if err := b.writeProfile(ctx, workspace, p, got); err != nil {
		return false, err
	}
	return true, nil
}

// stamp returns labels with the owner label added.
func (b *Backend) stamp(labels map[string]string) map[string]string {
	if b.owner == "" {
		return labels
	}
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	out[execution.LabelInstall] = b.owner
	return out
}

// check returns ErrForeign when labels name another install.
func (b *Backend) check(op, kind, name string, labels map[string]string) error {
	got := labels[execution.LabelInstall]
	if b.owner == "" || got == "" || got == b.owner {
		return nil
	}
	return &execution.Error{Op: op, Kind: execution.ErrForeign, Err: fmt.Errorf(
		"%s %q belongs to install %s, not to this one (%s)", kind, name, got, b.owner)}
}

// guardSandbox refuses a sandbox another install owns. A missing sandbox passes: the operation
// itself reports it, or, for a delete, treats it as done.
func (b *Backend) guardSandbox(ctx context.Context, workspace, name string) error {
	if b.owner == "" {
		return nil
	}
	_, err := b.GetSandbox(ctx, workspace, name) // refuses a foreign sandbox with ErrForeign
	if errors.Is(err, execution.ErrNotFound) {
		return nil
	}
	return err
}
