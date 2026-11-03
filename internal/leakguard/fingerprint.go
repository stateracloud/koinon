// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"unicode"
)

// Fingerprint is the keyed hash of one k-gram of canonical text.
type Fingerprint uint64

// Params are the winnowing parameters of a taint store (see the package documentation).
type Params struct {
	// Threshold is the shortest canonical match that is always caught (default 24).
	Threshold int
	// Window is the winnowing window w (default 4); the k-gram length is Threshold-Window+1.
	Window int
}

// Bounds of Params.
const (
	DefaultThreshold = 24
	DefaultWindow    = 4
	MinThreshold     = 12
	MaxThreshold     = 256
	minK             = 8
	// MinKeyBytes is the shortest fingerprint key accepted.
	MinKeyBytes = 16
)

// DefaultParams returns threshold 24 (ADR 0084 decision 3) and window 4, so k = 21.
func DefaultParams() Params { return Params{Threshold: DefaultThreshold, Window: DefaultWindow} }

// K returns the k-gram length, Threshold - Window + 1.
func (p Params) K() int { return p.Threshold - p.Window + 1 }

// Validate reports whether p can be used.
func (p Params) Validate() error {
	if p.Threshold < MinThreshold || p.Threshold > MaxThreshold {
		return fmt.Errorf("leakguard: threshold %d is outside %d..%d", p.Threshold, MinThreshold, MaxThreshold)
	}
	if p.Window < 1 || p.K() < minK {
		return fmt.Errorf("leakguard: window %d leaves a k-gram of %d characters; it needs at least %d", p.Window, p.K(), minK)
	}
	return nil
}

// Fingerprinter computes the keyed fingerprints of one scope (one home generation). It is safe
// for concurrent use.
type Fingerprinter struct {
	p     Params
	k     int
	block cipher.Block
	base  uint64 // polynomial base, derived from the key
	pow   uint64 // base^(k-1) mod mersenne61
}

// NewFingerprinter returns a fingerprinter for key and p. The key is the scope's secret (at
// least MinKeyBytes); fingerprints of one key do not match those of another.
func NewFingerprinter(key []byte, p Params) (*Fingerprinter, error) {
	if len(key) < MinKeyBytes {
		return nil, errors.New("leakguard: the fingerprint key is shorter than 16 bytes")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derive(key, "leakguard fingerprint aes v1")[:16])
	if err != nil {
		return nil, err
	}
	b := binary.LittleEndian.Uint64(derive(key, "leakguard fingerprint base v1")) % mersenne61
	if b < 1<<20 {
		b += 1 << 20
	}
	f := &Fingerprinter{p: p, k: p.K(), block: block, base: b}
	f.pow = 1
	for range f.k - 1 {
		f.pow = mulMod(f.pow, f.base)
	}
	return f, nil
}

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Params returns the fingerprinter's parameters.
func (f *Fingerprinter) Params() Params { return f.p }

// Winnow returns the winnowed fingerprints of text, the set a feed adds to the store: text is
// normalised first, and each fingerprint appears once.
func (f *Fingerprinter) Winnow(text string) []Fingerprint {
	return f.WinnowCanonical(Normalise(text))
}

// WinnowCanonical is Winnow on an already normalised text.
func (f *Fingerprinter) WinnowCanonical(c Canonical) []Fingerprint {
	all := f.kgrams(c.Runes, nil)
	if len(all) == 0 {
		return nil
	}
	w := f.p.Window
	seen := make(map[Fingerprint]struct{}, 2*len(all)/(w+1)+1)
	out := make([]Fingerprint, 0, 2*len(all)/(w+1)+1)
	pick := func(fp Fingerprint) {
		if _, ok := seen[fp]; !ok {
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	if len(all) <= w {
		pick(minRightmost(all))
		return out
	}
	// robust winnowing: in each window keep the minimum, the rightmost one on a tie, and record
	// it when the kept position changes
	last := -1
	for i := 0; i+w <= len(all); i++ {
		m := i
		for j := i + 1; j < i+w; j++ {
			if all[j] <= all[m] {
				m = j
			}
		}
		if m != last {
			pick(all[m])
			last = m
		}
	}
	return out
}

func minRightmost(fps []Fingerprint) Fingerprint {
	m := fps[0]
	for _, fp := range fps[1:] {
		if fp <= m {
			m = fp
		}
	}
	return m
}

// kgrams appends the fingerprint of every k-gram of r to out: len(r)-k+1 of them.
func (f *Fingerprinter) kgrams(r []rune, out []Fingerprint) []Fingerprint {
	k := f.k
	if len(r) < k {
		return out
	}
	var h uint64
	for i := range k {
		h = addMod(mulMod(h, f.base), symbol(r[i]))
	}
	var in, enc [aes.BlockSize]byte
	in[8], in[9] = byte(k&0xff), byte(k>>8&0xff)
	seal := func(h uint64) Fingerprint {
		binary.LittleEndian.PutUint64(in[:8], h)
		f.block.Encrypt(enc[:], in[:])
		return Fingerprint(binary.LittleEndian.Uint64(enc[:8]))
	}
	out = append(out, seal(h))
	for i := k; i < len(r); i++ {
		h = subMod(h, mulMod(symbol(r[i-k]), f.pow))
		h = addMod(mulMod(h, f.base), symbol(r[i]))
		out = append(out, seal(h))
	}
	return out
}

const mersenne61 = 1<<61 - 1

// symbol maps a rune to a nonzero value below the modulus.
func symbol(r rune) uint64 {
	if r < 0 || r > unicode.MaxRune {
		return 1
	}
	return uint64(r) + 2
}

func mulMod(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	// a, b < 2^61, so the product is below 2^122: split it at bit 61
	r := (hi<<3 | lo>>61) + lo&mersenne61 // below 2^62, so at most two reductions
	for r >= mersenne61 {
		r -= mersenne61
	}
	return r
}

func addMod(a, b uint64) uint64 {
	r := a + b
	if r >= mersenne61 {
		r -= mersenne61
	}
	return r
}

func subMod(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return a + mersenne61 - b
}
