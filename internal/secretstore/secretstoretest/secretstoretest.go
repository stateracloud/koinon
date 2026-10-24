// SPDX-License-Identifier: Apache-2.0

// Package secretstoretest gives tests a keyring with random keys.
package secretstoretest

import (
	"crypto/rand"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// Keyring returns a keyring with one random KEK "test-1", active.
func Keyring() *secretstore.Keyring {
	return KeyringWith("test-1")
}

// KeyringWith returns a keyring with random KEKs, the first one active.
func KeyringWith(ids ...string) *secretstore.Keyring {
	keys := map[string][]byte{}
	for _, id := range ids {
		k := make([]byte, secretstore.KeySize)
		_, _ = rand.Read(k)
		keys[id] = k
	}
	ring, err := secretstore.NewKeyring(keys, ids[0])
	if err != nil {
		panic(err)
	}
	return ring
}
