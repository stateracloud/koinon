// SPDX-License-Identifier: Apache-2.0

package actortoken

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublicKeyExt is the file name extension of a verifier key file.
const PublicKeyExt = ".pub"

// maxKeyFile bounds one key file. A PEM Ed25519 public key is about 113 bytes.
const maxKeyFile = 4 << 10

// LoadPublicKeys reads the verifier keys of dir: one file per key, named <kid>.pub, that holds one PEM
// "PUBLIC KEY" block (PKIX) of an Ed25519 key. Names that start with a dot are skipped, so the
// ..data links of a mounted Secret or ConfigMap do not count. Any other file, a key of another type or
// an empty directory is an error: the gateway must not start with a key set it only half read.
//
// The directory holds public keys only. A private key in it is refused, so a wrong mount cannot put a
// minting key into the gateway.
func LoadPublicKeys(dir string) (map[string]ed25519.PublicKey, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("actortoken: read key directory: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		kid, ok := strings.CutSuffix(name, PublicKeyExt)
		if !ok || kid == "" {
			return nil, fmt.Errorf("actortoken: key file %q must be named <key id>%s", name, PublicKeyExt)
		}
		k, err := readPublicKey(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("actortoken: key file %q: %w", name, err)
		}
		keys[kid] = k
	}
	if len(keys) == 0 {
		return nil, errors.New("actortoken: the key directory holds no key")
	}
	return keys, nil
}

func readPublicKey(file string) (ed25519.PublicKey, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxKeyFile {
		return nil, errors.New("not a regular file of at most 4 KiB")
	}
	b, err := os.ReadFile(file) //nolint:gosec // a file of the operator's key directory
	if err != nil {
		return nil, err
	}
	return ParsePublicKey(b)
}

// ParsePublicKey reads one PEM "PUBLIC KEY" block that holds an Ed25519 key. Text after the block is
// refused.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return nil, errors.New("no PEM PUBLIC KEY block (a private key is never accepted here)")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("text after the PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("the PEM block is not a PKIX public key")
	}
	ed, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the key is not an Ed25519 key")
	}
	return ed, nil
}

// SeedHexLen is the length of a minting key file: the 32-byte Ed25519 seed as hex.
const SeedHexLen = 2 * ed25519.SeedSize

// LoadSeedKey reads the minting key of a minter: one file that holds the Ed25519 seed as 64 hex
// characters (surrounding space is ignored). It is the form that koinon-api ensure-secrets writes
// (ensuresecrets.KindEd25519Key). The error never holds the file's text.
func LoadSeedKey(file string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("actortoken: minting key: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxKeyFile {
		return nil, errors.New("actortoken: minting key: not a regular file of at most 4 KiB")
	}
	b, err := os.ReadFile(file) //nolint:gosec // the operator's mounted key file
	if err != nil {
		return nil, fmt.Errorf("actortoken: minting key: %w", err)
	}
	return ParseSeedKey(b)
}

// ParseSeedKey reads an Ed25519 seed written as 64 hex characters.
func ParseSeedKey(b []byte) (ed25519.PrivateKey, error) {
	s := strings.TrimSpace(string(b))
	seed, err := hex.DecodeString(s)
	if err != nil || len(s) != SeedHexLen {
		return nil, fmt.Errorf("actortoken: minting key: want %d hex characters", SeedHexLen)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// MarshalPublicKey returns the PEM form that ParsePublicKey reads.
func MarshalPublicKey(k ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
