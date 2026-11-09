// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// HasLostRule reports whether keys forget knows what to do with a row of c.
func HasLostRule(c Column) bool {
	_, ok := lostRules[c]
	return ok
}

// RewrapWith is Rewrap with seal in place of the keyring's rewrap: a non-nil error of seal stands for a
// new envelope that could not be made (a KMS or a sealer that fails). A nil error leaves the row as it is.
func (t *Tool) RewrapWith(ctx context.Context, seal func(env secretstore.Envelope) error) error {
	return t.each(ctx, "rewrapped", "rewrap", ActionRewrap, func(_ *secretstore.Keyring, _ secretstore.Binding, env secretstore.Envelope, _ string) (*secretstore.Envelope, error) {
		return nil, seal(env)
	})
}

// FailureKind is failureKind.
func FailureKind(err error) string { return failureKind(err) }
