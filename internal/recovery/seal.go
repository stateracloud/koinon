// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"bytes"
	"fmt"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// ParseRecipient parses an age X25519 recipient ("age1...") the requester made for one request.
func ParseRecipient(s string) (*age.X25519Recipient, error) {
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return nil, fmt.Errorf("recipientPublicKey: not an age X25519 recipient: %w", err)
	}
	return r, nil
}

// Seal encrypts plain to the recipient and returns the armored ciphertext. Only the holder of the
// private key, which never leaves the requester's machine, can open it (`age -d`). This is how the
// result of a request is "readable by the requester only" without a Secret, a Role or a RoleBinding
// (ADR 0110 decision 6).
func Seal(r *age.X25519Recipient, plain []byte) (string, error) {
	var buf bytes.Buffer
	a := armor.NewWriter(&buf)
	w, err := age.Encrypt(a, r)
	if err != nil {
		return "", fmt.Errorf("seal result: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return "", fmt.Errorf("seal result: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("seal result: %w", err)
	}
	if err := a.Close(); err != nil {
		return "", fmt.Errorf("seal result: %w", err)
	}
	return buf.String(), nil
}
