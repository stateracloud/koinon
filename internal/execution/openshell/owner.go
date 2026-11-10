// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/stateracloud/koinon/internal/execution"
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
// and homes, are not recreated, and stay unmarked (they are shared, idempotent records). Until the
// live install has reconciled once, a second install on the gateway can still touch its unlabeled
// objects; docs/site guides/upgrading.md says so. An empty id turns
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
