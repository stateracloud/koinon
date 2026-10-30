// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"strings"
	"unicode/utf8"
)

// A view is the text read another way: with JSON escapes decoded, with percent escapes decoded,
// or with fullwidth characters folded to ASCII. Detectors run on the original text and on each
// view that differs from it, and a span found in a view is mapped back to the original bytes, so
// an escaped credential is replaced where it stands.
type view struct {
	s string
	// pos[i] is where view byte i starts in the original; pos[len(s)] is the original length.
	// The bytes of a multi-byte character after the first map to where its source ends, so a
	// span that ends inside one covers the whole source.
	pos []int
}

// toOriginal maps the view range [a, b) to the original text.
func (v *view) toOriginal(a, b int) (int, int) { return v.pos[a], v.pos[b] }

// maxViews bounds how many readings of one text are examined. Each reading is one decoding step
// (JSON escapes, percent escapes, fullwidth folding) applied to an earlier reading, so JSON escaped
// three times, URL-encoded text inside it, or a fullwidth character inside escaped JSON are all reached.
const (
	maxViews  = 14
	maxRounds = 6
)

// detectViews appends the spans found in the views of in. detect is the detector set.
func detectViews(in string, detect func(string, []span) []span, spans []span) []span {
	if !needsViews(in) { // no escape, no fullwidth form: no other reading
		return spans
	}
	type item struct {
		v     *view
		round int
	}
	root := &view{s: in}
	frontier := []item{{root, 0}}
	seen := map[string]bool{in: true}
	for made := 0; len(frontier) > 0 && made < maxViews; {
		it := frontier[0]
		frontier = frontier[1:]
		if it.round >= maxRounds {
			continue
		}
		for _, mk := range [...]func(string) (*view, bool){unescapeView, percentView, foldView} {
			v, ok := mk(it.v.s)
			if !ok || seen[v.s] || made >= maxViews {
				continue
			}
			seen[v.s] = true
			made++
			if it.v != root {
				v = it.v.compose(v)
			}
			for _, sp := range detect(v.s, nil) {
				a, b := v.toOriginal(sp.start, sp.end)
				if b > a {
					spans = append(spans, span{a, b, sp.kind})
				}
			}
			frontier = append(frontier, item{v, it.round + 1})
		}
	}
	return spans
}

// compose returns the view of v whose positions refer to the original text of v's source.
func (v *view) compose(next *view) *view {
	pos := make([]int, len(next.pos))
	for i, p := range next.pos {
		pos[i] = v.pos[p]
	}
	return &view{next.s, pos}
}

// unescapeView decodes the escapes of a JSON string: \n \t \r \b \f \" \/ \\ and \uXXXX with
// surrogate pairs.
func unescapeView(in string) (*view, bool) {
	if !strings.Contains(in, `\`) {
		return nil, false
	}
	var b strings.Builder
	pos := make([]int, 0, len(in)+1)
	changed := false
	for i := 0; i < len(in); {
		if in[i] != '\\' || i+1 >= len(in) {
			b.WriteByte(in[i])
			pos = append(pos, i)
			i++
			continue
		}
		var r rune
		n := 2
		switch c := in[i+1]; c {
		case 'n':
			r = '\n'
		case 't':
			r = '\t'
		case 'r':
			r = '\r'
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case '"', '\\', '/':
			r = rune(c)
		case 'u':
			u, ok := hex4(in, i+2)
			if !ok {
				b.WriteByte('\\')
				pos = append(pos, i)
				i++
				continue
			}
			r, n = u, 6
			if u >= 0xD800 && u < 0xDC00 && i+12 <= len(in) && in[i+6] == '\\' && in[i+7] == 'u' {
				if lo, ok := hex4(in, i+8); ok && lo >= 0xDC00 && lo < 0xE000 {
					r, n = 0x10000+(u-0xD800)<<10+(lo-0xDC00), 12
				}
			}
		default:
			b.WriteByte('\\')
			pos = append(pos, i)
			i++
			continue
		}
		changed = true
		var buf [4]byte
		w := utf8.EncodeRune(buf[:], r)
		b.Write(buf[:w])
		pos = append(pos, i)
		for k := 1; k < w; k++ {
			pos = append(pos, i+n)
		}
		i += n
	}
	if !changed {
		return nil, false
	}
	pos = append(pos, len(in))
	return &view{b.String(), pos}, true
}

func hex4(s string, i int) (rune, bool) {
	if i+4 > len(s) {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[i : i+4]) {
		switch {
		case isDigit(c):
			r = r<<4 | rune(c-'0')
		case 'a' <= c && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case 'A' <= c && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// percentView decodes %XX escapes.
func percentView(in string) (*view, bool) {
	i := strings.IndexByte(in, '%')
	for i >= 0 && (i+2 >= len(in) || !isHex(in[i+1]) || !isHex(in[i+2])) {
		j := strings.IndexByte(in[i+1:], '%')
		if j < 0 {
			return nil, false
		}
		i += 1 + j
	}
	if i < 0 {
		return nil, false
	}
	var b strings.Builder
	pos := make([]int, 0, len(in)+1)
	for k := 0; k < len(in); {
		if in[k] == '%' && k+2 < len(in) && isHex(in[k+1]) && isHex(in[k+2]) {
			b.WriteByte(unhex(in[k+1])<<4 | unhex(in[k+2]))
			pos = append(pos, k)
			k += 3
			continue
		}
		b.WriteByte(in[k])
		pos = append(pos, k)
		k++
	}
	pos = append(pos, len(in))
	return &view{b.String(), pos}, true
}

func unhex(c byte) byte {
	switch {
	case isDigit(c):
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// foldView folds the fullwidth forms U+FF01 to U+FF5E (＠ for @, ． for ., fullwidth letters and
// digits) and the ideographic full stop to ASCII, for detection only.
func foldView(in string) (*view, bool) {
	if !strings.Contains(in, "\xef\xbc") && !strings.Contains(in, "\xef\xbd") && !strings.Contains(in, "\xe3\x80\x82") {
		return nil, false
	}
	var b strings.Builder
	pos := make([]int, 0, len(in)+1)
	changed := false
	for i := 0; i < len(in); {
		r, w := utf8.DecodeRuneInString(in[i:])
		out := r
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			out = r - 0xFF01 + 0x21
		case r == 0x3002:
			out = '.'
		}
		if out != r {
			changed = true
			b.WriteByte(byte(out))
			pos = append(pos, i)
			i += w
			// the folded character is one byte; a later span end maps to the next start
			continue
		}
		b.WriteString(in[i : i+w])
		for k := 0; k < w; k++ {
			pos = append(pos, i+k)
		}
		i += w
	}
	if !changed {
		return nil, false
	}
	pos = append(pos, len(in))
	return &view{b.String(), pos}, true
}

// needsViews reports whether in holds a backslash, a percent sign or a lead byte of a fullwidth
// or ideographic character, the only things the views change.
func needsViews(in string) bool {
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case '\\', '%', 0xef, 0xe3:
			return true
		}
	}
	return false
}
