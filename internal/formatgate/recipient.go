// SPDX-License-Identifier: Apache-2.0

package formatgate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// KeySource is the writer's source of the key to seal to (mcpservers.RecipientSource).
type KeySource interface {
	Current() (secretstore.RecipientPublicKey, error)
}

// DefaultHold is how long the gate must stay open without a break before GatedKey gives the key. It is
// longer than a pod's drain (95 s) and a report's TTL, so a pod of release N-1 that a rollout is
// still stopping has gone before the first v2 value is written (ADR 0097 12.5 step 1).
const DefaultHold = 2 * time.Minute

// GatedKey gives the inner key only while the gate has been open, without a break, for Hold. While it is
// shut, Current returns an error that wraps ErrNotReady, and the writer keeps to the old format. Use a
// pointer: it remembers since when the gate has been open.
type GatedKey struct {
	Inner  KeySource
	Gate   Gate
	Format Format
	// Hold is DefaultHold when zero. Now is time.Now when nil.
	Hold time.Duration
	Now  func() time.Time

	mu    sync.Mutex
	since time.Time
}

// Current implements the key source.
func (g *GatedKey) Current() (secretstore.RecipientPublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	hold := g.Hold
	if hold == 0 {
		hold = DefaultHold
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.Gate.Ready(ctx, g.Format); err != nil {
		g.since = time.Time{}
		return secretstore.RecipientPublicKey{}, err
	}
	if g.since.IsZero() {
		g.since = now()
	}
	if now().Sub(g.since) < hold {
		return secretstore.RecipientPublicKey{}, fmt.Errorf("%w: open for less than %s", ErrNotReady, hold)
	}
	return g.Inner.Current()
}
