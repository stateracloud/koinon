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
// An object without the label was made before it existed and counts as this install's; OpenShell
// keeps labels immutable, so it gets the label when the reconciler recreates it. An empty id turns
// the checks off (agentd, which only reads and execs in sandboxes the reconciler recorded). It
// returns b.
func (b *Backend) WithOwner(id string) *Backend {
	b.owner = id
	return b
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
func (b *Backend) guardSandbox(ctx context.Context, op, workspace, name string) error {
	if b.owner == "" {
		return nil
	}
	sb, err := b.GetSandbox(ctx, workspace, name)
	if errors.Is(err, execution.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return b.check(op, "sandbox", name, sb.Labels)
}
