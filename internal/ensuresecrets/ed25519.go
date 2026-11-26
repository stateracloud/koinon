// SPDX-License-Identifier: Apache-2.0

package ensuresecrets

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// PublicKeyKey is the data key that holds the public key of a KindEd25519Key Secret, as a PEM
// "PUBLIC KEY" block. A verifier mounts only this key, never the seed under KeyKey: koinon-gateway
// checks actor tokens with it (ADR 0120 1.2, #1086 part C2), so it cannot sign one.
const PublicKeyKey = "public.pem"

// newEd25519Data returns the data of a new KindEd25519Key Secret: a random seed under KeyKey and
// its public key under PublicKeyKey.
func newEd25519Data() (map[string][]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	seedHex := []byte(hex.EncodeToString(seed))
	pemPub, err := ed25519PublicPEM(seedHex)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{KeyKey: seedHex, PublicKeyKey: pemPub}, nil
}

// ed25519PublicPEM returns the PEM public key of a seed in KeyKey form (64 lowercase hex characters).
func ed25519PublicPEM(seedHex []byte) ([]byte, error) {
	key, err := actortoken.ParseSeedKey(seedHex)
	if err != nil {
		return nil, fmt.Errorf("its %q key is not 64 hex characters", KeyKey)
	}
	pub, _ := key.Public().(ed25519.PublicKey)
	return actortoken.MarshalPublicKey(pub)
}

// checkEd25519Data checks a kept KindEd25519Key Secret. The seed under KeyKey must be 64 lowercase
// hex characters. A Secret without PublicKeyKey (made before the key had one, for example the
// guest key of v0.1.0-rc.30) returns the public key to add. A Secret with PublicKeyKey must hold the
// seed's public key, so a verifier never checks tokens against a key the signer does not use. A bad
// Secret is an error and is never changed. Errors never hold key text.
func checkEd25519Data(data map[string][]byte) (addPublic []byte, err error) {
	seedHex := data[KeyKey]
	if !hexKey.Match(seedHex) {
		return nil, fmt.Errorf("its %q key is not 64 hex characters", KeyKey)
	}
	want, err := ed25519PublicPEM(seedHex)
	if err != nil {
		return nil, err
	}
	got, ok := data[PublicKeyKey]
	if !ok {
		return want, nil
	}
	pub, err := actortoken.ParsePublicKey(got)
	if err != nil {
		return nil, fmt.Errorf("its %q key is not a PEM Ed25519 public key", PublicKeyKey)
	}
	wantPub, err := actortoken.ParsePublicKey(want)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pub, wantPub) {
		return nil, fmt.Errorf("its %q key is not the public key of its %q key", PublicKeyKey, KeyKey)
	}
	return nil, nil
}

// errSeedChanged is the error of a public-key patch whose test failed: the Secret changed after it
// was read.
var errSeedChanged = errors.New("it changed while the public key was added; run again")
