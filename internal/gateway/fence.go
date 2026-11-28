// SPDX-License-Identifier: Apache-2.0

package gateway

import "context"

// A fence is how the caller of a driver says "my right to change the gateway may have ended". The
// reconciler's leadership rests on a Postgres session, and a silent connection loss (a hard node loss
// or a partition) is seen only after a bound (issue 635, ADR 0076 amendment 2026-11-18). A driver that
// takes a step another leader cannot undo (deleting objects) checks the fence of its ctx first, so
// the old leader's destructive step stops at the earliest moment the reconciler doubts its lease,
// not only when the lead context is cancelled. The leader term (package reconciler/leaderterm) fences
// the database writes; the database cannot fence a Kubernetes delete, so this fence stays for them
// until issue 1465 ends it.

type fenceKey struct{}

// WithFence returns a ctx that carries check. check returns nil while the caller may still change
// the gateway and an error once it may not.
func WithFence(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, fenceKey{}, check)
}

// CheckFence returns the error of the fence on ctx, or nil when ctx has no fence or the fence holds.
// A driver calls it before each step that deletes. It also returns ctx's own error.
func CheckFence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(fenceKey{}).(func() error); ok && check != nil {
		return check()
	}
	return nil
}
