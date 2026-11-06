// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
)

// CheckWork is Check, and also returns the bytes of work the call was charged: the same on every
// machine, unlike its duration.
func (c TaintChecker) CheckWork(ctx context.Context, scope Scope, fp *Fingerprinter, args []string) (Decision, int64, error) {
	var w int64
	d, err := c.check(ctx, scope, fp, args, nil, &w)
	return d, w, err
}
