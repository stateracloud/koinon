// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// FakeKMS is a secretstore.Wrapper that stands in for a remote KMS in tests: AES-GCM under a fixed,
// obviously fake key, with switches for an outage and for a decrypt-only identity. It passes
// WrapperContract, which a real adapter's tests run too.
type FakeKMS struct {
	// Name is the KeyID (default "koinon-test").
	Name string
	// Down makes every call fail with secretstore.ErrUnreachable.
	Down atomic.Bool
	// DecryptOnly makes Wrap fail as a decrypt-only identity would.
	DecryptOnly atomic.Bool

	once  sync.Once
	aead  cipher.AEAD
	wraps atomic.Int64
}

// FakeKMSBackend is FakeKMS's backend name.
const FakeKMSBackend = "fakekms"

func (f *FakeKMS) gcm() cipher.AEAD {
	f.once.Do(func() {
		b, err := aes.NewCipher(bytes.Repeat([]byte("w"), secretstore.KeySize)) // obviously fake
		if err != nil {
			panic(err)
		}
		if f.aead, err = cipher.NewGCM(b); err != nil {
			panic(err)
		}
	})
	return f.aead
}

// Wraps counts the successful Wrap calls.
func (f *FakeKMS) Wraps() int64 { return f.wraps.Load() }

// Backend implements secretstore.Wrapper.
func (f *FakeKMS) Backend() string { return FakeKMSBackend }

// KeyID implements secretstore.Wrapper.
func (f *FakeKMS) KeyID() string {
	if f.Name == "" {
		return "koinon-test"
	}
	return f.Name
}

func (f *FakeKMS) reach() error {
	if f.Down.Load() {
		return fmt.Errorf("%w: fake KMS is down", secretstore.ErrUnreachable)
	}
	return nil
}

// Wrap implements secretstore.Wrapper.
func (f *FakeKMS) Wrap(_ context.Context, kek, aad []byte) ([]byte, error) {
	if err := f.reach(); err != nil {
		return nil, err
	}
	if f.DecryptOnly.Load() {
		return nil, fmt.Errorf("%w: this identity may only decrypt", secretstore.ErrUnreachable)
	}
	a := f.gcm()
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	f.wraps.Add(1)
	return a.Seal(nonce, nonce, kek, aad), nil
}

// Unwrap implements secretstore.Wrapper.
func (f *FakeKMS) Unwrap(_ context.Context, wrapped, aad []byte) ([]byte, error) {
	if err := f.reach(); err != nil {
		return nil, err
	}
	a := f.gcm()
	if len(wrapped) < a.NonceSize() {
		return nil, secretstore.ErrOpen
	}
	out, err := a.Open(nil, wrapped[:a.NonceSize()], wrapped[a.NonceSize():], aad)
	if err != nil {
		return nil, secretstore.ErrOpen
	}
	return out, nil
}

// Probe implements secretstore.Wrapper.
func (f *FakeKMS) Probe(context.Context) error { return f.reach() }
