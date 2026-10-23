// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: the second recommended option of RFC 9106 (t=3,
// m=64 MiB, p=4). They are encoded in every hash, so they can be raised later
// without invalidating stored passwords.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16

	// MinPasswordLength is the minimum length, in characters, of a new
	// password.
	MinPasswordLength = 12
	// maxPasswordBytes bounds the work an attacker can request per guess.
	maxPasswordBytes = 1024
)

var (
	errMalformedHash = errors.New("malformed password hash")
	// dummyHash is verified against when a username does not exist, so the
	// response time does not reveal which usernames exist.
	dummyHash = sync.OnceValue(func() string { return mustHash("koinon-dummy-password") })
)

// HashPassword returns an argon2id hash in PHC string format:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the PHC-encoded hash.
func VerifyPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformedHash
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, errMalformedHash
	}
	if memory == 0 || time == 0 || threads == 0 || memory > 1<<21 || time > 16 {
		return false, errMalformedHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errMalformedHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false, errMalformedHash
	}
	if len(password) > maxPasswordBytes {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // len(want) <= 64
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// ValidateNewPassword enforces the password policy for a password change.
func ValidateNewPassword(password string) error {
	if !utf8.ValidString(password) {
		return errors.New("password must be valid UTF-8")
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("password must have at least %d characters", MinPasswordLength)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	return nil
}

func mustHash(password string) string {
	h, err := HashPassword(password)
	if err != nil {
		panic(err)
	}
	return h
}
