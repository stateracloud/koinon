// SPDX-License-Identifier: Apache-2.0

package fakeate

import (
	"context"
	"sync"
)

// ActorNames is an in-memory substrate.ActorNameLedger for tests. It lives as long as the value: give two
// drivers the same value to model a restart or a second replica. Production uses the database ledger of
// package actornames, which outlives a process.
type ActorNames struct {
	mu    sync.Mutex
	names map[[2]string]bool
	// Err, when set, is what every claim returns.
	Err error
}

// NewActorNames returns an empty ledger.
func NewActorNames() *ActorNames { return &ActorNames{names: map[[2]string]bool{}} }

// ClaimActorName implements substrate.ActorNameLedger.
func (l *ActorNames) ClaimActorName(_ context.Context, atespace, name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Err != nil {
		return false, l.Err
	}
	k := [2]string{atespace, name}
	if l.names[k] {
		return false, nil
	}
	l.names[k] = true
	return true, nil
}
