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

// KindEd25519Key is Spec.Kind of a Secret that holds an Ed25519 signing key: the seed as 64 hex
// characters under SeedKey and the public key as a PEM "PUBLIC KEY" block under PublicKeyKey. The
// actor token's key (ADR 0120 1.2, #1086 part C2): koinon-agentd mounts only the seed, the gateway
// only the public key.
const KindEd25519Key = "ed25519-key"

// SeedKey is the data key that holds the seed of a KindEd25519Key Secret.
const SeedKey = "seed"

// PublicKeyKey is the data key that holds the public key of a KindEd25519Key Secret.
const PublicKeyKey = "public.pem"

func newEd25519Data() (map[string][]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	pub, _ := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	pemPub, err := actortoken.MarshalPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{SeedKey: []byte(hex.EncodeToString(seed)), PublicKeyKey: pemPub}, nil
}

// checkEd25519Data checks a kept Secret: the seed parses and the public key is the seed's, so the
// gateway never checks tokens against a key agentd does not sign with. Errors never hold key text.
func checkEd25519Data(data map[string][]byte) error {
	key, err := actortoken.ParseSeedKey(data[SeedKey])
	if err != nil {
		return fmt.Errorf("its %q key is not 64 hex characters", SeedKey)
	}
	pub, err := actortoken.ParsePublicKey(data[PublicKeyKey])
	if err != nil {
		return fmt.Errorf("its %q key is not a PEM Ed25519 public key", PublicKeyKey)
	}
	want, _ := key.Public().(ed25519.PublicKey)
	if !bytes.Equal(pub, want) {
		return errors.New("its public key is not the public key of its seed")
	}
	return nil
}
