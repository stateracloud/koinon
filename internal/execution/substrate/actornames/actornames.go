// SPDX-License-Identifier: Apache-2.0

// Package actornames is the database ledger of Substrate actor names (issue #1092): the
// [substrate.ActorNameLedger] that production drivers use. A claim is a row in substrate_actor_names, so it
// outlives a restart of the driver and every replica of koinon-reconciler sees it. A row belongs to the org
// whose workspace is the atespace, and goes with that org.
package actornames

import (
	"context"
	"fmt"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution/substrate"
)

// Store claims actor names in the Koinon database.
type Store struct{ q *store.Queries }

var _ substrate.ActorNameLedger = (*Store)(nil)

// New returns a ledger on db (a pool in production).
func New(db store.DBTX) *Store { return &Store{q: store.New(db)} }

// ClaimActorName implements [substrate.ActorNameLedger]. The org is the one whose workspace (org_workspaces)
// is the atespace; an atespace of no org is an error, so the driver creates nothing. The insert is atomic: of
// two drivers that claim one name at once, exactly one gets true.
func (s *Store) ClaimActorName(ctx context.Context, atespace, name string) (bool, error) {
	r, err := s.q.ClaimSubstrateActorName(ctx, store.ClaimSubstrateActorNameParams{Atespace: atespace, Name: name})
	if err != nil {
		return false, fmt.Errorf("actornames: claim %s/%s: %w", atespace, name, err)
	}
	if !r.Known {
		return false, fmt.Errorf("actornames: claim %s/%s: no org has the workspace %s", atespace, name, atespace)
	}
	return r.Claimed, nil
}
