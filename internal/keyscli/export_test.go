// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

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

// ProbeEpochs is probeEpochs for the given agents (org, agent), with its answer keyed the same
// way: a spec can name a target whose repository row is gone, as when the agent was deleted
// after the command read its targets.
func (t *Tool) ProbeEpochs(ctx context.Context, targets [][2]pgtype.UUID) (map[[2]pgtype.UUID]int32, error) {
	hs := make([]homeTarget, 0, len(targets))
	for _, x := range targets {
		hs = append(hs, homeTarget{org: x[0], agent: x[1]})
	}
	got, err := t.probeEpochs(ctx, hs)
	if err != nil {
		return nil, err
	}
	out := make(map[[2]pgtype.UUID]int32, len(got))
	for k, v := range got {
		out[[2]pgtype.UUID{k.org, k.agent}] = v
	}
	return out, nil
}
