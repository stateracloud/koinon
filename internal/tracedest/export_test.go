// SPDX-License-Identifier: Apache-2.0

package tracedest

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// WouldTrace exposes the sampling decision for a given draw.
func (s *Service) WouldTrace(ctx context.Context, org pgtype.UUID, draw float64) (bool, error) {
	return s.wouldTrace(ctx, store.New(s.pool), org, draw)
}
