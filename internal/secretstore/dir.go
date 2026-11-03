// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ActiveFile is the name, in the KEK directory or Secret, of the file that
// holds the active KEK's ID. Every other regular file is a KEK named by its
// file name; its content is the base64 of 32 random bytes (a [Local] KEK) or a
// KEK wrapped by a KMS ([StoredKEK.Encode]).
const ActiveFile = "active"

// ParseKeySet parses the data of the KEK Secret: files named by KEK ID with
// stored KEKs, plus ActiveFile. It unwraps nothing ([Wrappers.UnwrapAll]).
func ParseKeySet(data map[string][]byte) (keys map[string]StoredKEK, active string, err error) {
	keys = map[string]StoredKEK{}
	var errs []error
	for name, v := range data {
		if name == ActiveFile {
			active = strings.TrimSpace(string(v))
			continue
		}
		if strings.HasPrefix(name, "..") {
			continue // the kubelet's atomic-update directories
		}
		if !ValidKEKID(name) {
			errs = append(errs, fmt.Errorf("%q is not a valid key-encryption key ID", name))
			continue
		}
		s, err := decodeStored(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("key %q: %w", name, err)) // decodeStored never quotes the value
			continue
		}
		keys[name] = s
	}
	if err := errors.Join(errs...); err != nil {
		return nil, "", err
	}
	if active == "" {
		return nil, "", fmt.Errorf("no %q entry naming the active key-encryption key", ActiveFile)
	}
	if _, ok := keys[active]; !ok {
		return nil, "", fmt.Errorf("the active key-encryption key %q is not among the keys", active)
	}
	return keys, active, nil
}

// decodeKey reads base64 (44 characters) or hex (64 characters) of KeySize bytes.
func decodeKey(s string) ([]byte, bool) {
	if len(s) == 2*KeySize {
		if raw, err := hex.DecodeString(s); err == nil {
			return raw, true
		}
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	return raw, err == nil && len(raw) == KeySize
}

// EncodeKey returns the Secret value of a new key.
func EncodeKey(key []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(key)) }

// ReadDir reads a KEK directory (the mounted Secret). It unwraps nothing.
func ReadDir(dir string) (keys map[string]StoredKEK, active string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("secretstore: read %s: %w", dir, err)
	}
	data := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), "..") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the operator-configured KEK directory; follows the kubelet's symlinks
		if err != nil {
			return nil, "", fmt.Errorf("secretstore: read %s: %w", e.Name(), err)
		}
		data[e.Name()] = b
	}
	keys, active, err = ParseKeySet(data)
	if err != nil {
		return nil, "", fmt.Errorf("secretstore: %s: %w", dir, err)
	}
	return keys, active, nil
}

// openDir reads and unwraps a KEK directory.
func openDir(ctx context.Context, dir string, ws Wrappers, obs []Observer) (map[string][]byte, string, error) {
	stored, active, err := ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	keys, err := ws.UnwrapAll(ctx, stored)
	notify(ctx, obs, NewLoaded(stored, active, keys, err))
	if err != nil {
		return nil, "", fmt.Errorf("secretstore: %s: %w", dir, err)
	}
	return keys, active, nil
}

// LoadDir returns a keyring of the KEK directory, its KEKs unwrapped by ws (the zero Wrappers
// read [Local] KEKs only). Each observer is told what the load found.
func LoadDir(ctx context.Context, dir string, ws Wrappers, obs ...Observer) (*Keyring, error) {
	keys, active, err := openDir(ctx, dir, ws, obs)
	if err != nil {
		return nil, err
	}
	defer clearKeys(keys)
	return NewKeyring(keys, active)
}

// clearKeys zeroes key bytes once a keyring holds its own copies (the AEADs).
func clearKeys(keys map[string][]byte) {
	for _, k := range keys {
		clear(k)
	}
}

// Reload re-reads the directory into k. A failed reload keeps the old keys.
// Callers run it on a timer so a mounted Secret's changes (kubelet propagates
// them within about a minute) apply without a restart.
func (k *Keyring) Reload(ctx context.Context, dir string, ws Wrappers, obs ...Observer) error {
	keys, active, err := openDir(ctx, dir, ws, obs)
	if err != nil {
		return err
	}
	defer clearKeys(keys)
	return k.Replace(keys, active)
}

// WatchDir reloads the directory every interval until ctx ends, so a changed Secret applies
// without a restart. A failed reload is logged and the old keys stay. Each observer is told what
// every reload found, including a reload whose KEKs could not be unwrapped.
func (k *Keyring) WatchDir(ctx context.Context, dir string, ws Wrappers, interval time.Duration, log *slog.Logger, obs ...Observer) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := k.Active()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := k.Reload(ctx, dir, ws, obs...); err != nil {
				log.Warn("reloading the key-encryption keys failed; keeping the previous ones", "err", err)
				continue
			}
			if a := k.Active(); a != prev {
				log.Info("the active key-encryption key changed", "from", prev, "to", a)
				prev = a
			}
		}
	}
}
