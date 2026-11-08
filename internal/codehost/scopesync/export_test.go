// SPDX-License-Identifier: Apache-2.0

package scopesync

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// SetMaxRepos lowers the per-connection bound, so a spec reaches it with a few pages.
func SetMaxRepos(s *Syncer, n int) { s.maxRepos = n }

// SetMinAge sets how recent a reading a round skips; 0 reads every connection.
func SetMinAge(s *Syncer, d time.Duration) { s.minAge = d }

// SetTimeout sets the bound on one connection's reading.
func SetTimeout(s *Syncer, d time.Duration) { s.timeout = d }

// RoundLock names the advisory lock a round's leader holds.
const RoundLock = roundLock

// Sync is one unqueued reading, so a spec can see its error; the product asks through Request.
func (s *Syncer) Sync(ctx context.Context, row store.Connection, actor audit.Actor) error {
	return s.read(ctx, row, actor)
}
