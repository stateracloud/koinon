// SPDX-License-Identifier: Apache-2.0

// Package remotekms is what the adapters of a remote KMS share (OpenBao transit old issue 416, AWS KMS old issue 417):
// the per-call deadline, the plaintext frame that binds the associated data, and the error rules
// of secretstore.Wrapper. An adapter only speaks its KMS's protocol and classifies its answers.
//
// The rules, for every adapter:
//   - Every call ends by min(the caller's deadline, the adapter's own timeout) ([Bound]).
//   - The KMS cannot be used (no answer, a refused sign-in, no right on the key, the key missing, a
//     sealed or overloaded server): the error wraps secretstore.ErrUnreachable ([Unreachable]).
//   - The KMS answered and the ciphertext does not open (modified, another key, other associated
//     data): secretstore.ErrOpen.
//   - Errors name the backend and the key, never key material, a token or a payload.
package remotekms

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// DefaultTimeout is an adapter's own bound on one call when its configuration sets none. It is
// below secretstore.CallTimeout, so an adapter gives up (and says why) before the caller does.
const DefaultTimeout = 5 * time.Second

// Bound returns ctx bounded by d as well (DefaultTimeout when d <= 0).
func Bound(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = DefaultTimeout
	}
	return context.WithTimeout(ctx, d)
}

// Unreachable wraps err in secretstore.ErrUnreachable with what was being done, unless it already
// is one.
func Unreachable(what string, err error) error {
	if errors.Is(err, secretstore.ErrUnreachable) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %w", secretstore.ErrUnreachable, what, err)
}

// The frame is what the KMS encrypts: a version byte, the SHA-256 of the associated data and the
// key. It binds the associated data on any KMS, including one that has no associated data of its
// own or ignores it (an older Vault transit), so a wrapped KEK copied under another ID never
// unwraps. The KMS's authenticated encryption covers the whole frame.
const (
	frameV1  = 1
	frameLen = 1 + sha256.Size
)

// Frame returns the plaintext to send to the KMS for kek and aad.
func Frame(kek, aad []byte) []byte {
	sum := sha256.Sum256(aad)
	out := make([]byte, 0, frameLen+len(kek))
	out = append(out, frameV1)
	out = append(out, sum[:]...)
	return append(out, kek...)
}

// Unframe returns the key in a plaintext the KMS decrypted, or secretstore.ErrOpen when it was
// framed for other associated data or is no frame. It clears plain either way.
func Unframe(plain, aad []byte) ([]byte, error) {
	defer clear(plain)
	sum := sha256.Sum256(aad)
	if len(plain) <= frameLen || plain[0] != frameV1 || subtle.ConstantTimeCompare(plain[1:frameLen], sum[:]) != 1 {
		return nil, secretstore.ErrOpen
	}
	return bytes.Clone(plain[frameLen:]), nil
}
