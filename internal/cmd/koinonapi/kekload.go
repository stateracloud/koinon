// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
)

func loadKeyring(ctx context.Context, store keyscli.Store) (*secretstore.Keyring, error) {
	ks, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return secretstore.NewKeyring(ks.Keys, ks.Active)
}

// watchKeyring re-reads the KEK Secret every interval, so "keys add" and "keys activate" reach a
// running koinon-api without a restart. A failed read keeps the old keys.
func watchKeyring(ctx context.Context, ring *secretstore.Keyring, store keyscli.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := ring.Active()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ks, err := store.Load(ctx)
			if err == nil {
				err = ring.Replace(ks.Keys, ks.Active)
			}
			if err != nil {
				slog.Warn("reloading the key-encryption keys failed; keeping the previous ones", "err", err)
				continue
			}
			if a := ring.Active(); a != prev {
				slog.Info("the active key-encryption key changed", "from", prev, "to", a)
				prev = a
			}
		}
	}
}
