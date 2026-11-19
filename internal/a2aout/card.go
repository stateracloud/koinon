// SPDX-License-Identifier: Apache-2.0

package a2aout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// MaxCardBytes bounds the agent card that Koinon stores and hashes. The card is data from another party.
const MaxCardBytes = 256 << 10

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validHash(h string) bool { return hashPattern.MatchString(h) }

// CanonicalCard returns the canonical JSON of an agent card and its SHA-256 (hex): the keys sorted, no
// insignificant white space, numbers kept as the agent wrote them. The pin of a remote agent is this hash
// (ADR 0123 decision 11). A card that is not one JSON object, or is larger than [MaxCardBytes], is an
// [ErrCard].
func CanonicalCard(raw []byte) (canonical []byte, hash string, err error) {
	if len(raw) > MaxCardBytes {
		return nil, "", fmt.Errorf("%w: the card is larger than %d KiB", ErrCard, MaxCardBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "", fmt.Errorf("%w: the card is not JSON", ErrCard)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "", fmt.Errorf("%w: the card is not one JSON value", ErrCard)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, "", fmt.Errorf("%w: the card is not a JSON object", ErrCard)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, "", fmt.Errorf("%w: the card cannot be encoded", ErrCard)
	}
	canonical = bytes.TrimRight(buf.Bytes(), "\n")
	if len(canonical) > MaxCardBytes {
		return nil, "", fmt.Errorf("%w: the card is larger than %d KiB", ErrCard, MaxCardBytes>>10)
	}
	sum := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(sum[:]), nil
}

// Card is an agent card as Koinon pins it.
type Card struct {
	// JSON is the canonical JSON of the card. It is data from another party: for admins only, never for a
	// model, a log or an error.
	JSON string
	// Hash is the SHA-256 (hex) of JSON.
	Hash string
}

// FetchCard reads the agent card of the target and returns it with its hash. It uses the same HTTP client,
// the same egress policy and the same credential rules as [New], and it does not check Options.CardHash:
// registration and the health check call it to learn what the card is now. The card must offer a JSON-RPC
// interface on the host of the target, as [New] requires, so an agent that cannot be called is refused at
// registration.
func FetchCard(ctx context.Context, o Options) (*Card, error) {
	hc, base, o, err := open(o)
	if err != nil {
		return nil, err
	}
	if err := o.Policy.CheckStrict(ctx, o.Target.URL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEgressDenied, err)
	}
	rctx, cancel := context.WithTimeout(ctx, o.RequestTimeout)
	defer cancel()
	card, raw, err := resolveCard(rctx, hc, o.Target.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCard, err)
	}
	if len(sameOrigin(card.SupportedInterfaces, base)) == 0 {
		return nil, fmt.Errorf("%w: no JSON-RPC interface on %s", ErrCardHost, base.Host)
	}
	canonical, hash, err := CanonicalCard(raw)
	if err != nil {
		return nil, err
	}
	return &Card{JSON: string(canonical), Hash: hash}, nil
}
