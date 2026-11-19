// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/audit"
)

func testKey(t *testing.T, origin string, fill byte) *audit.SigningKey {
	t.Helper()
	k, err := audit.NewSigningKey(origin, bytes.Repeat([]byte{fill}, audit.SeedSize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testNote(origin string) audit.Note {
	h := sha256.Sum256([]byte("head"))
	return audit.Note{Origin: origin, ChainSeq: 42, Hash: h[:]}
}

func TestNoteRoundTrip(t *testing.T) {
	origin := audit.NoteOrigin("11111111-1111-1111-1111-111111111111")
	if !strings.HasPrefix(origin, "koinon.dev/install/") || !strings.HasSuffix(origin, "/audit") {
		t.Fatalf("origin %q", origin)
	}
	k := testKey(t, origin, 1)
	raw, err := k.Sign(testNote(origin))
	if err != nil {
		t.Fatal(err)
	}
	n, pub, err := audit.VerifyNote(raw, []audit.PublicKey{k.Public()})
	if err != nil {
		t.Fatal(err)
	}
	if n.ChainSeq != 42 || n.Origin != origin || !bytes.Equal(n.Hash, testNote(origin).Hash) || pub.ID() != k.Public().ID() {
		t.Fatalf("round trip changed the note: %+v", n)
	}
	// The public key survives its text form.
	p2, err := audit.ParsePublicKey(k.Public().String())
	if err != nil || p2.ID() != pub.ID() {
		t.Fatalf("parse public key: %v", err)
	}
}

func TestNoteTamperedFails(t *testing.T) {
	origin := audit.NoteOrigin("22222222-2222-2222-2222-222222222222")
	k := testKey(t, origin, 2)
	raw, err := k.Sign(testNote(origin))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte("\n42\n"), []byte("\n41\n"), 1)
	if _, _, err := audit.VerifyNote(tampered, []audit.PublicKey{k.Public()}); !errors.Is(err, audit.ErrNoteBadSignature) {
		t.Fatalf("a changed chain_seq must fail the signature, got %v", err)
	}
	for _, bad := range [][]byte{nil, []byte("x"), []byte("a\nb\nc\n\n"), bytes.TrimSuffix(raw, []byte("=\n"))} {
		if _, _, err := audit.VerifyNote(bad, []audit.PublicKey{k.Public()}); err == nil {
			t.Fatalf("garbage %q verified", bad)
		}
	}
}

func TestNoteWrongAndRotatedKey(t *testing.T) {
	origin := audit.NoteOrigin("33333333-3333-3333-3333-333333333333")
	oldKey, newKey, other := testKey(t, origin, 3), testKey(t, origin, 4), testKey(t, origin, 5)
	raw, err := oldKey.Sign(testNote(origin))
	if err != nil {
		t.Fatal(err)
	}
	// An unknown key fails.
	if _, _, err := audit.VerifyNote(raw, []audit.PublicKey{other.Public()}); !errors.Is(err, audit.ErrNoteUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, _, err := audit.VerifyNote(raw, nil); !errors.Is(err, audit.ErrNoteUnknownKey) {
		t.Fatalf("no key: %v", err)
	}
	// A rotated old key still verifies its notes, next to the new key.
	if _, pub, err := audit.VerifyNote(raw, []audit.PublicKey{newKey.Public(), oldKey.Public()}); err != nil || pub.ID() != oldKey.Public().ID() {
		t.Fatalf("rotated key: %v", err)
	}
	// A key of the same name and ID cannot be forged: the ID follows the key bytes.
	if oldKey.Public().ID() == newKey.Public().ID() {
		t.Fatal("two keys share an ID")
	}
}

func TestSignRefusesOtherOriginAndBadNote(t *testing.T) {
	k := testKey(t, audit.NoteOrigin("44444444-4444-4444-4444-444444444444"), 6)
	if _, err := k.Sign(testNote(audit.NoteOrigin("55555555-5555-5555-5555-555555555555"))); err == nil {
		t.Fatal("a key signed for another install")
	}
	n := testNote(k.Name)
	n.ChainSeq = 0
	if _, err := k.Sign(n); err == nil {
		t.Fatal("signed chain_seq 0")
	}
	if _, err := audit.NewSigningKey(k.Name, []byte("short")); err == nil {
		t.Fatal("accepted a short seed")
	}
}

func TestParsePublicKeyRejects(t *testing.T) {
	k := testKey(t, "koinon.dev/install/x/audit", 7).Public()
	parts := strings.Split(k.String(), "+")
	for _, bad := range []string{"", "a+b", parts[0] + "+deadbeef+" + parts[2], parts[0] + "+" + parts[1] + "+AAAA"} {
		if _, err := audit.ParsePublicKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// Standard base64 holds plus signs, so a public key line must split at the first two only.
func TestPublicKeyWithPlusInBase64RoundTrips(t *testing.T) {
	found := 0
	for fill := range byte(200) {
		k := testKey(t, "koinon.dev/install/x/audit", fill).Public()
		parts := strings.SplitN(k.String(), "+", 3)
		if !strings.Contains(parts[2], "+") {
			continue
		}
		found++
		p, err := audit.ParsePublicKey(k.String())
		if err != nil || p.ID() != k.ID() {
			t.Fatalf("seed byte %d: %v", fill, err)
		}
	}
	if found == 0 {
		t.Fatal("no test key had a plus sign in its base64")
	}
}
