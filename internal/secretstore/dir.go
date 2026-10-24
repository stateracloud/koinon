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
// file name; its content is the base64 of 32 random bytes.
const ActiveFile = "active"

// ParseKeySet parses the data of the KEK Secret: files named by KEK ID with
// base64 key material, plus ActiveFile.
func ParseKeySet(data map[string][]byte) (keys map[string][]byte, active string, err error) {
	keys = map[string][]byte{}
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
		raw, ok := decodeKey(strings.TrimSpace(string(v)))
		if !ok {
			// Never quote the value.
			errs = append(errs, fmt.Errorf("key %q: want the base64 or 64 hex characters of %d random bytes", name, KeySize))
			continue
		}
		keys[name] = raw
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

// ReadDir reads a KEK directory (the mounted Secret).
func ReadDir(dir string) (keys map[string][]byte, active string, err error) {
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

// LoadDir returns a keyring of the KEK directory.
func LoadDir(dir string) (*Keyring, error) {
	keys, active, err := ReadDir(dir)
	if err != nil {
		return nil, err
	}
	return NewKeyring(keys, active)
}

// Reload re-reads the directory into k. A failed reload keeps the old keys.
// Callers run it on a timer so a mounted Secret's changes (kubelet propagates
// them within about a minute) apply without a restart.
func (k *Keyring) Reload(dir string) error {
	keys, active, err := ReadDir(dir)
	if err != nil {
		return err
	}
	return k.Replace(keys, active)
}

// WatchDir reloads the directory every interval until ctx ends, so a changed Secret applies
// without a restart. A failed reload is logged and the old keys stay.
func (k *Keyring) WatchDir(ctx context.Context, dir string, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := k.Active()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := k.Reload(dir); err != nil {
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
