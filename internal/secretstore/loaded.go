// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"time"
)

// Loaded is what one read of a key set found, handed to an [Observer] after every load and reload
// that got as far as reading the stored KEKs. It holds no key material.
type Loaded struct {
	// Stored is each KEK as the Secret holds it.
	Stored map[string]StoredKEK
	Active string
	// Fingerprints is [Fingerprint] of each unwrapped KEK; nil when unwrapping failed (Err).
	Fingerprints map[string]string
	// ActivatedAt is when Active became active, if the source knows it.
	ActivatedAt time.Time
	// Err is why the KEKs could not be unwrapped (wrapping [ErrUnreachable] when the KMS did not
	// answer), or nil.
	Err error
}

// NewLoaded builds a [Loaded] from the stored KEKs and the result of unwrapping them. keys may be
// nil when err is not.
func NewLoaded(stored map[string]StoredKEK, active string, keys map[string][]byte, err error) Loaded {
	l := Loaded{Stored: stored, Active: active, Err: err}
	if err == nil {
		l.Fingerprints = make(map[string]string, len(keys))
		for id, k := range keys {
			l.Fingerprints[id] = Fingerprint(k)
		}
	}
	return l
}

// Observer is told about each load of a key set, so the caller can record fingerprints and probe the
// wrapping key without the keyring knowing about either. It runs on the loading goroutine, so it
// must return promptly and must not call back into the keyring.
type Observer func(ctx context.Context, l Loaded)

func notify(ctx context.Context, obs []Observer, l Loaded) {
	for _, o := range obs {
		if o != nil {
			o(ctx, l)
		}
	}
}
