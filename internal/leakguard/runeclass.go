// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"sync"
	"unicode"
	"unicode/utf8"
)

// Classes of a character for the text tests (textual, best, fromUTF16). The Unicode tables are
// slow to ask for every character of a megabyte, so the classes of the Basic Multilingual Plane
// are computed once.
const (
	clsPrint     uint8 = 1 << iota // printable, or a space
	clsWord                        // a letter, a digit or a space
	clsPunct                       // punctuation or a symbol
	clsInvis                       // a control, format or combining character: invisible, an attacker's filler
	clsMark                        // a format or combining mark (invisible, but graphic text carries it)
	clsFold                        // folds to an ASCII letter or digit in the matching form (see Normalise)
	clsSpace                       // a space
	clsPunctOnly                   // punctuation (not a symbol)
)

var runeClasses = sync.OnceValue(func() *[1 << 16]uint8 {
	var t [1 << 16]uint8
	var buf [16]rune
	for u := range t {
		r := rune(u)
		var c uint8
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			c |= clsPrint
		}
		if unicode.IsSpace(r) {
			c |= clsWord | clsSpace
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			c |= clsWord
		}
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			c |= clsPunct
		}
		if unicode.IsPunct(r) {
			c |= clsPunctOnly
		}
		if unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) {
			c |= clsInvis | clsMark
		}
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			c |= clsInvis
		}
		if r < utf8.RuneSelf {
			if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
				c |= clsFold
			}
		} else if r < 0xd800 || r >= 0xe000 {
			if foldsToASCII(appendCanonRune(buf[:0], r, 0)) {
				c |= clsFold
			}
		}
		t[u] = c
	}
	return &t
})

// classOf returns the classes of r. An invalid byte has none of them but clsInvis.
func classOf(t *[1 << 16]uint8, r rune) uint8 {
	switch {
	case r < 0:
		return clsInvis
	case r < 1<<16:
		return t[r]
	}
	var c uint8
	if unicode.IsPrint(r) {
		c |= clsPrint
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		c |= clsWord
	}
	if unicode.IsPunct(r) || unicode.IsSymbol(r) {
		c |= clsPunct
	}
	if unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) {
		c |= clsInvis | clsMark
	}
	var buf [16]rune
	if foldsToASCII(appendCanonRune(buf[:0], r, 0)) {
		c |= clsFold
	}
	return c
}
