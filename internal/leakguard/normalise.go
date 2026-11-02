// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Canonical is the matching form of a text (see the package documentation): folded letters and
// digits only. Off[i] is the byte offset in the source text of the character rune i came from,
// so a match can be shown in the text that would leave.
type Canonical struct {
	Runes []rune
	Off   []int
}

// String returns the canonical text.
func (c Canonical) String() string { return string(c.Runes) }

// Normalise returns the canonical form of s.
func Normalise(s string) Canonical {
	var c Canonical
	c.Runes = make([]rune, 0, len(s))
	c.Off = make([]int, 0, len(s))
	normaliseInto(&c, s)
	return c
}

// normaliseInto appends the canonical form of s to c.
func normaliseInto(c *Canonical, s string) {
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			switch {
			case 'a' <= b && b <= 'z', '0' <= b && b <= '9':
				c.Runes = append(c.Runes, rune(b))
				c.Off = append(c.Off, i)
			case 'A' <= b && b <= 'Z':
				c.Runes = append(c.Runes, rune(b+'a'-'A'))
				c.Off = append(c.Off, i)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError {
			n := len(c.Runes)
			c.Runes = appendCanonRune(c.Runes, r, 0)
			for range len(c.Runes) - n {
				c.Off = append(c.Off, i)
			}
		}
		i += size
	}
}

// maxCanonDepth bounds the recursion of decomposition and folding; real chains are at most two
// steps (a compatibility decomposition, then a case fold).
const maxCanonDepth = 4

// appendCanonRune appends the canonical runes of the non-separator r.
func appendCanonRune(out []rune, r rune, depth int) []rune {
	if r < utf8.RuneSelf {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			return append(out, r)
		case 'A' <= r && r <= 'Z':
			return append(out, r+'a'-'A')
		}
		return out
	}
	if depth >= maxCanonDepth {
		return out
	}
	if m, ok := confusables[r]; ok {
		return append(out, m...)
	}
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], r)
	if d := norm.NFKD.Properties(buf[:n]).Decomposition(); d != nil {
		for _, dr := range string(d) {
			out = appendCanonRune(out, dr, depth+1)
		}
		return out
	}
	if f := unicode.ToLower(unicode.ToUpper(r)); f != r {
		return appendCanonRune(out, f, depth+1)
	}
	if d, ok := digitValue(r); ok {
		return append(out, '0'+d)
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return append(out, r)
	}
	return out
}

// digitValue returns the value of a decimal digit of any script (Unicode Nd), which the
// matching form writes as the ASCII digit. Every Nd range starts at a zero and runs in tens.
func digitValue(r rune) (rune, bool) {
	for _, rg := range unicode.Nd.R16 {
		if lo, hi := rune(rg.Lo), rune(rg.Hi); lo <= r && r <= hi && rg.Stride == 1 {
			return (r - lo) % 10, true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		// Nd code points are below U+20000, so they fit a rune
		if lo, hi := rune(rg.Lo), rune(rg.Hi); lo <= r && r <= hi && rg.Stride == 1 { //nolint:gosec // see above
			return (r - lo) % 10, true
		}
	}
	return 0, false
}

// confusables folds look-alike letters of other scripts, and Latin letters without a
// decomposition, to Latin. The table is keyed by the original rune, case included, because the
// look-alike of a Greek or Cyrillic letter depends on its case (Greek "Ν" looks like "N", "ν"
// like "v"). It is a conservative subset of the Unicode confusables (UTS 39) for the letters
// an attacker would swap into Latin text; it never maps digits.
var confusables = map[rune][]rune{
	// Latin letters without a decomposition
	'ß': []rune("ss"), 'ẞ': []rune("ss"), 'æ': []rune("ae"), 'Æ': []rune("ae"),
	'œ': []rune("oe"), 'Œ': []rune("oe"), 'ø': {'o'}, 'Ø': {'o'}, 'đ': {'d'}, 'Đ': {'d'},
	'ł': {'l'}, 'Ł': {'l'}, 'ħ': {'h'}, 'Ħ': {'h'}, 'ı': {'i'}, 'ȷ': {'j'}, 'ɩ': {'i'},
	'ɑ': {'a'}, 'ɡ': {'g'}, 'ʟ': {'l'}, 'ɪ': {'i'}, 'ᴏ': {'o'}, 'ᴄ': {'c'}, 'ᴠ': {'v'},
	// Cyrillic, upper case
	'А': {'a'}, 'В': {'b'}, 'Е': {'e'}, 'К': {'k'}, 'М': {'m'}, 'Н': {'h'}, 'О': {'o'},
	'Р': {'p'}, 'С': {'c'}, 'Т': {'t'}, 'Х': {'x'}, 'У': {'y'}, 'І': {'i'}, 'Ј': {'j'},
	'Ѕ': {'s'}, 'Ү': {'y'}, 'Һ': {'h'}, 'Ԛ': {'q'}, 'Ԝ': {'w'}, 'Ӏ': {'l'}, 'Ԁ': {'d'},
	// Cyrillic, lower case
	'а': {'a'}, 'в': {'b'}, 'е': {'e'}, 'к': {'k'}, 'м': {'m'}, 'н': {'h'}, 'о': {'o'},
	'р': {'p'}, 'с': {'c'}, 'т': {'t'}, 'х': {'x'}, 'у': {'y'}, 'і': {'i'}, 'ј': {'j'},
	'ѕ': {'s'}, 'ү': {'y'}, 'һ': {'h'}, 'ԛ': {'q'}, 'ԝ': {'w'}, 'ӏ': {'l'}, 'ԁ': {'d'},
	// Greek, upper case
	'Α': {'a'}, 'Β': {'b'}, 'Ε': {'e'}, 'Ζ': {'z'}, 'Η': {'h'}, 'Ι': {'i'}, 'Κ': {'k'},
	'Μ': {'m'}, 'Ν': {'n'}, 'Ο': {'o'}, 'Ρ': {'p'}, 'Τ': {'t'}, 'Υ': {'y'}, 'Χ': {'x'},
	// Greek, lower case
	'α': {'a'}, 'ε': {'e'}, 'ι': {'i'}, 'κ': {'k'}, 'ν': {'v'}, 'ο': {'o'}, 'ρ': {'p'},
	'υ': {'u'}, 'χ': {'x'},
	// Armenian
	'օ': {'o'}, 'ս': {'u'}, 'ց': {'g'},
}
