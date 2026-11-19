// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stateracloud/koinon/internal/project"
)

// Signed checkpoint notes (ADR 0100 decision 4.2). A checkpoint is a signed note in Koinon's own
// format. It borrows the layout of a C2SP signed note (origin line, body, a blank line, signature lines)
// but it is not a tlog checkpoint: its body is the head of a hash chain, not a Merkle tree root, so it
// supports no inclusion or consistency proofs and tools built for tlog checkpoints do not verify it.
//
//	<origin>                      koinon.dev/install/<install id>/audit
//	<chain_seq of the head>       decimal
//	<base64 of the head hash>     standard base64, 32 bytes
//	(blank line)
//	— <key name> <base64 of 4 key-hash bytes and the 64 byte Ed25519 signature>
//
// The signature covers the three body lines with their newlines. The key hash is the first 4 bytes of
// sha256(key name, "\n", 0x01, public key), as in C2SP signed-note, so one note can carry the signatures
// of several keys and a verifier picks the lines of keys it knows.

const (
	// noteSigType is the signature type byte of Ed25519 in a C2SP signed note.
	noteSigType = 0x01
	// noteDash is the em dash that opens a signature line.
	noteDash = "— "
	// maxNoteSize bounds what ParseNote reads: a note is a few hundred bytes.
	maxNoteSize = 16 << 10
)

// Note is the body of a checkpoint: the install chain's head at one chain_seq.
type Note struct {
	// Origin names what the head belongs to: NoteOrigin of the install.
	Origin   string
	ChainSeq int64
	// Hash is the 32-byte hash of the install chain row at ChainSeq.
	Hash []byte
}

// NoteOrigin is the origin line of an install's checkpoints: the project domain, the install ID and "audit".
func NoteOrigin(installID string) string {
	return project.Domain + "/install/" + installID + "/audit"
}

// Text is the signed body: three lines, each ending in a newline.
func (n Note) Text() string {
	return n.Origin + "\n" + strconv.FormatInt(n.ChainSeq, 10) + "\n" + base64.StdEncoding.EncodeToString(n.Hash) + "\n"
}

// SigningKey is an Ed25519 key that signs checkpoints. Its name is the origin of the install, so a note
// names which install the key speaks for; the key hash tells keys of one install apart.
type SigningKey struct {
	Name string
	priv ed25519.PrivateKey
}

// SeedSize is the size of the private seed a SigningKey is made from.
const SeedSize = ed25519.SeedSize

// NewSigningKey makes a SigningKey from a 32-byte seed. It copies the seed.
func NewSigningKey(name string, seed []byte) (*SigningKey, error) {
	if len(seed) != SeedSize {
		return nil, fmt.Errorf("the audit signing seed has %d bytes, want %d", len(seed), SeedSize)
	}
	if err := checkKeyName(name); err != nil {
		return nil, err
	}
	return &SigningKey{Name: name, priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// Public is the public half of k.
func (k *SigningKey) Public() PublicKey {
	return PublicKey{Name: k.Name, Key: k.priv.Public().(ed25519.PublicKey)}
}

// Sign signs n and returns the whole note, with one signature line. A note whose origin is not the key's
// name is refused: a key speaks for one install.
func (k *SigningKey) Sign(n Note) ([]byte, error) {
	if n.Origin != k.Name {
		return nil, fmt.Errorf("the key %q does not sign for origin %q", k.Name, n.Origin)
	}
	if len(n.Hash) != sha256.Size || n.ChainSeq < 1 {
		return nil, errors.New("a checkpoint names a chain_seq of 1 or more and a 32-byte hash")
	}
	text := n.Text()
	sig := ed25519.Sign(k.priv, []byte(text))
	pub := k.Public()
	line := append(pub.keyHash(), sig...)
	return []byte(text + "\n" + noteDash + k.Name + " " + base64.StdEncoding.EncodeToString(line) + "\n"), nil
}

// PublicKey is the public half of a SigningKey, what a verifier keeps off the cluster.
type PublicKey struct {
	Name string
	Key  ed25519.PublicKey
}

func (p PublicKey) keyHash() []byte {
	h := sha256.Sum256(append(append([]byte(p.Name+"\n"), noteSigType), p.Key...))
	return h[:4]
}

// ID is the key hash as 8 lower-case hex characters. It tells the keys of one install apart (rotation).
func (p PublicKey) ID() string { return hex.EncodeToString(p.keyHash()) }

// String is the key as one line a verifier keeps: "<name>+<key id>+<base64 of 0x01 and the public key>",
// the vkey form of C2SP signed-note.
func (p PublicKey) String() string {
	return p.Name + "+" + p.ID() + "+" + base64.StdEncoding.EncodeToString(append([]byte{noteSigType}, p.Key...))
}

// ParsePublicKey reads the String form and checks the key ID against the key.
func ParsePublicKey(s string) (PublicKey, error) {
	s = strings.TrimSpace(s)
	// The name and the ID have no plus sign, but standard base64 can: split at the first two only.
	parts := strings.SplitN(s, "+", 3)
	if len(parts) != 3 {
		return PublicKey{}, errors.New("a public key is <name>+<id>+<base64>")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(raw) != 1+ed25519.PublicKeySize || raw[0] != noteSigType {
		return PublicKey{}, errors.New("a public key holds the type byte 0x01 and a 32-byte Ed25519 key in base64")
	}
	p := PublicKey{Name: parts[0], Key: ed25519.PublicKey(raw[1:])}
	if err := checkKeyName(p.Name); err != nil {
		return PublicKey{}, err
	}
	if p.ID() != parts[1] {
		return PublicKey{}, fmt.Errorf("the key ID %q does not match the key (it is %s)", parts[1], p.ID())
	}
	return p, nil
}

func checkKeyName(name string) error {
	if name == "" || strings.ContainsAny(name, " \t\r\n+") {
		return fmt.Errorf("the key name %q is empty or has a space or a plus sign", name)
	}
	return nil
}

// Errors of VerifyNote.
var (
	// ErrNoteFormat: the bytes are not a checkpoint note.
	ErrNoteFormat = errors.New("audit checkpoint: not a checkpoint note")
	// ErrNoteUnknownKey: no signature line of the note is by a key the verifier knows.
	ErrNoteUnknownKey = errors.New("audit checkpoint: not signed by a known key")
	// ErrNoteBadSignature: a known key's signature line does not verify: the note was changed or forged.
	ErrNoteBadSignature = errors.New("audit checkpoint: the signature does not verify")
)

// ParseNote reads the body of a note and its signature lines without checking any signature.
func ParseNote(raw []byte) (Note, []NoteSignature, error) {
	if len(raw) == 0 || len(raw) > maxNoteSize {
		return Note{}, nil, fmt.Errorf("%w: %d bytes", ErrNoteFormat, len(raw))
	}
	body, sigs, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		return Note{}, nil, fmt.Errorf("%w: no blank line before the signatures", ErrNoteFormat)
	}
	text := string(body) + "\n"
	lines := strings.Split(string(body), "\n")
	if len(lines) != 3 {
		return Note{}, nil, fmt.Errorf("%w: the body has %d lines, want 3", ErrNoteFormat, len(lines))
	}
	seq, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != lines[1] {
		return Note{}, nil, fmt.Errorf("%w: chain_seq %q", ErrNoteFormat, lines[1])
	}
	hash, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil || len(hash) != sha256.Size {
		return Note{}, nil, fmt.Errorf("%w: the hash is not 32 bytes of base64", ErrNoteFormat)
	}
	n := Note{Origin: lines[0], ChainSeq: seq, Hash: hash}
	if n.Origin == "" {
		return Note{}, nil, fmt.Errorf("%w: no origin", ErrNoteFormat)
	}
	var out []NoteSignature
	for _, l := range strings.Split(strings.TrimSuffix(string(sigs), "\n"), "\n") {
		rest, ok := strings.CutPrefix(l, noteDash)
		name, b64, ok2 := strings.Cut(rest, " ")
		if !ok || !ok2 {
			return Note{}, nil, fmt.Errorf("%w: a signature line is \"— <name> <base64>\"", ErrNoteFormat)
		}
		blob, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(blob) != 4+ed25519.SignatureSize {
			return Note{}, nil, fmt.Errorf("%w: the signature of %q is not 4+64 bytes of base64", ErrNoteFormat, name)
		}
		out = append(out, NoteSignature{Name: name, keyHash: blob[:4], sig: blob[4:], text: text})
	}
	if len(out) == 0 {
		return Note{}, nil, fmt.Errorf("%w: no signature line", ErrNoteFormat)
	}
	return n, out, nil
}

// NoteSignature is one signature line of a note.
type NoteSignature struct {
	Name    string
	keyHash []byte
	sig     []byte
	text    string
}

// VerifyNote parses a note and checks it against the keys the verifier knows (a key is known when its
// name and key ID both match a signature line). It returns the note and the key that signed it. Any
// number of keys may be known: a rotated key stays valid for the checkpoints it signed. A signature line
// of an unknown key is ignored; a line of a known key that does not verify fails the note.
func VerifyNote(raw []byte, known []PublicKey) (Note, PublicKey, error) {
	n, sigs, err := ParseNote(raw)
	if err != nil {
		return Note{}, PublicKey{}, err
	}
	for _, s := range sigs {
		for _, k := range known {
			if k.Name != s.Name || !bytes.Equal(k.keyHash(), s.keyHash) {
				continue
			}
			if !ed25519.Verify(k.Key, []byte(s.text), s.sig) {
				return Note{}, PublicKey{}, fmt.Errorf("%w (key %s)", ErrNoteBadSignature, k.ID())
			}
			return n, k, nil
		}
	}
	return Note{}, PublicKey{}, ErrNoteUnknownKey
}
