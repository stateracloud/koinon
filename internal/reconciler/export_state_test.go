// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"

	"github.com/stateracloud/koinon/internal/gateway"
)

// PublishState runs the State stream publish of one pass on s.
func (r *Reconciler) PublishState(ctx context.Context, s gateway.State) { r.publishState(ctx, s) }
