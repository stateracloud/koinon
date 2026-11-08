// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// loaded reports a load of the key set to the observer, also when the KEKs could not be unwrapped
// (Store.Load then returns the stored KEKs without keys).
func loaded(ctx context.Context, ks keyscli.KeySet, err error, obs secretstore.Observer) {
	if obs == nil || ks.Stored == nil {
		return
	}
	l := secretstore.NewLoaded(ks.Stored, ks.Active, ks.Keys, err)
	l.ActivatedAt = ks.ActivatedAt
	obs(ctx, l)
}

func loadKeyring(ctx context.Context, store keyscli.Store, obs secretstore.Observer) (*secretstore.Keyring, error) {
	ks, err := store.Load(ctx)
	loaded(ctx, ks, err, obs)
	if err != nil {
		return nil, err
	}
	return secretstore.NewKeyring(ks.Keys, ks.Active)
}

// keySetSignature names a key set by its active KEK and its KEK IDs and fingerprints, never a key.
func keySetSignature(ks keyscli.KeySet) string {
	var b strings.Builder
	b.WriteString(ks.Active)
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		b.WriteString("\n" + id + " " + secretstore.Fingerprint(ks.Keys[id]))
	}
	return b.String()
}

// watchKeyring re-reads the KEK Secret every interval, so "keys add" and "keys activate" reach a
// running koinon-api without a restart. A failed read keeps the old keys. The observer is told
// about every read that got as far as the stored KEKs. When a read finds a key set other than the
// one it last saw (the first read included), changed runs, if set: koinon-api's lost-change check
// (keyAudit.observe), which loads the key set again under the advisory lock of the key events.
func watchKeyring(ctx context.Context, ring *secretstore.Keyring, store keyscli.Store, interval time.Duration, obs secretstore.Observer, changed func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := ring.Active()
	seen := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ks, err := store.Load(ctx)
			loaded(ctx, ks, err, obs)
			if err == nil {
				err = ring.Replace(ks.Keys, ks.Active)
			}
			if err != nil {
				slog.Warn("reloading the key-encryption keys failed; keeping the previous ones", "err", err)
				continue
			}
			sig := keySetSignature(ks)
			if a := ring.Active(); a != prev {
				slog.Info("the active key-encryption key changed", "from", prev, "to", a)
				prev = a
			}
			if sig != seen {
				seen = sig
				if changed != nil {
					changed(ctx)
				}
			}
		}
	}
}
