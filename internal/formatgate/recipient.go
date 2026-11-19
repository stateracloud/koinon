// SPDX-License-Identifier: Apache-2.0

package formatgate

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// KeySource is the writer's source of the key to seal to (mcpservers.RecipientSource).
type KeySource interface {
	Current() (secretstore.RecipientPublicKey, error)
}

// GatedKey gives the inner key only while the gate is open for Format. While it is shut, Current
// returns an error that wraps ErrNotReady, and the writer keeps to the old format.
type GatedKey struct {
	Inner  KeySource
	Gate   Gate
	Format Format
}

// Current implements the key source.
func (g GatedKey) Current() (secretstore.RecipientPublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Gate.Ready(ctx, g.Format); err != nil {
		return secretstore.RecipientPublicKey{}, err
	}
	return g.Inner.Current()
}
