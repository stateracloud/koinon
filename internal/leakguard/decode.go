// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"cmp"
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"html"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Variant is one decoding of a text: the text itself (Chain empty) or the result of one or more
// decoders, named in Chain in the order they ran ("percent>base64").
type Variant struct {
	Text  string
	Chain string
}

// Decode returns s and every decoding of it the leak guard matches, within the limits: up to
// MaxDepth nested decodings and MaxVariants variants. A budget that runs out, or text that still
// decodes at MaxDepth, is an error naming the reason; the caller fails closed on it.
func Decode(ctx context.Context, s string, lim Limits) ([]Variant, error) {
	lim = lim.withDefaults()
	return decodeAll(s, lim, newMeter(ctx, lim))
}

const (
	// minRun is the shortest base64, base32 or hex stream decoded (16 characters).
	minRun = 16
	// textualRun is the stretch of printable characters that makes a decoding text.
	textualRun = 12
	// chunkRun is the shortest encoded-looking chunk that triggers compaction.
	chunkRun = 8
)

type decoder struct {
	name string
	fn   func(string) []string
}

var decoders = []decoder{
	{name: "percent", fn: decodePercent},
	{name: "escape", fn: decodeEscapes},
	{name: "compact", fn: decodeCompact},
	{name: "base64", fn: decodeBase64},
	{name: "base32", fn: decodeBase32},
	{name: "hex", fn: decodeHex},
}

func decodeAll(s string, lim Limits, m *meter) ([]Variant, error) {
	type item struct {
		v     Variant
		depth int
	}
	seen := map[string]struct{}{s: {}}
	out := []Variant{{Text: s}}
	queue := []item{{Variant{Text: s}, 0}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		for _, d := range decoders {
			if err := m.add(len(it.v.Text)); err != nil {
				return nil, err
			}
			for _, child := range d.fn(it.v.Text) {
				if _, dup := seen[child]; dup {
					continue
				}
				if it.depth >= lim.MaxDepth {
					return nil, &budgetError{ReasonDecodeDepth, fmt.Sprintf("an argument is encoded more than %d levels deep; the tool call could not be checked", lim.MaxDepth)}
				}
				if len(out) >= lim.MaxVariants {
					return nil, &budgetError{ReasonBudgetVariants, fmt.Sprintf("an argument decodes in more than %d ways; the tool call could not be checked", lim.MaxVariants)}
				}
				seen[child] = struct{}{}
				chain := d.name
				if it.v.Chain != "" {
					chain = it.v.Chain + ">" + d.name
				}
				v := Variant{Text: child, Chain: chain}
				out = append(out, v)
				queue = append(queue, item{v, it.depth + 1})
			}
		}
	}
	return out, nil
}

// decodePercent undoes %HH escapes. It returns nothing when there is nothing to undo. "+" as a
// space needs no decoding: both are separators in the matching form.
func decodePercent(s string) []string {
	if !strings.Contains(s, "%") {
		return nil
	}
	if d, ok := percentDecode(s); ok {
		return []string{d}
	}
	return nil
}

func percentDecode(s string) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			if !changed {
				b.Grow(len(s))
				b.WriteString(s[:i])
				changed = true
			}
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		if changed {
			b.WriteByte(c)
		}
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

// decodeEscapes undoes backslash escapes (\xHH, \uHHHH, \UHHHHHHHH) and HTML character
// references (&#107;, &#x6b;, &amp;).
func decodeEscapes(s string) []string {
	if !strings.ContainsAny(s, `\&`) {
		return nil
	}
	out := s
	if strings.Contains(out, `\`) {
		out = unescapeBackslash(out)
	}
	if strings.Contains(out, "&") {
		out = html.UnescapeString(out)
	}
	if out == s {
		return nil
	}
	return []string{out}
}

func unescapeBackslash(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			n := 0
			switch s[i+1] {
			case 'x', 'X':
				n = 2
			case 'u':
				n = 4
			case 'U':
				n = 8
			}
			if n > 0 && i+2+n <= len(s) && allHex(s[i+2:i+2+n]) {
				var v rune
				for j := range n {
					v = v<<4 | rune(unhex(s[i+2+j]))
				}
				if n == 2 {
					b.WriteByte(byte(v & 0xff))
				} else {
					b.WriteRune(v)
				}
				i += 1 + n
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// decodeCompact decodes an encoded stream cut into chunks ("cGF5 cm9s", "6b6f6 96e"): it
// removes separators, whitespace, padding and \x or 0x prefixes, and returns what the base64,
// base32 and hex decoders make of the result. It only runs when the text has two
// encoded-looking chunks with nothing but separators between them.
func decodeCompact(s string) []string {
	if !chunked(s) {
		return nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '\\' || c == '0') && i+2 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') && isHex(s[i+2]) &&
			(c == '\\' || i == 0 || !isAlnumByte(s[i-1])) {
			i++
			continue
		}
		if isCompactSep(c) {
			continue
		}
		b.WriteByte(c)
	}
	c := b.String()
	if c == s {
		return nil
	}
	var out []string
	for _, fn := range []func(string) []string{decodeBase64, decodeBase32, decodeHex} {
		out = append(out, fn(c)...)
	}
	return out
}

// chunked reports whether s has two chunks of at least chunkRun base64 characters, each with a
// digit or both letter cases, separated by separators only.
func chunked(s string) bool {
	prevChunkEnd := -1
	for i := 0; i < len(s); {
		if !isBase64Char(s[i]) {
			i++
			continue
		}
		j := i
		var digit, upper, lower bool
		for j < len(s) && isBase64Char(s[j]) {
			c := s[j]
			digit = digit || '0' <= c && c <= '9'
			upper = upper || 'A' <= c && c <= 'Z'
			lower = lower || 'a' <= c && c <= 'z'
			j++
		}
		if j-i >= chunkRun && (digit || upper && lower) {
			if prevChunkEnd >= 0 && onlySeps(s[prevChunkEnd:i]) {
				return true
			}
			prevChunkEnd = j
		} else {
			prevChunkEnd = -1
		}
		i = j
	}
	return false
}

func onlySeps(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isCompactSep(s[i]) {
			return false
		}
	}
	return true
}

func isCompactSep(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\v', '\f', ':', ',', ';', '.', '|', '\'', '"', '=', '\\', '(', ')', '[', ']', '{', '}', '<', '>':
		return true
	}
	return false
}

// A blockCodec decodes one fixed-size block of an encoding; resync uses it to decode a stream
// at whatever alignment each part of it has.
type blockCodec struct {
	chars, bytes, minBlocks int
	block                   func(dst []byte, src string) bool
	partial                 func(src string) ([]byte, bool) // a short trailing block, optional
}

var (
	b64Codec = blockCodec{
		chars: 4, bytes: 3, minBlocks: 2,
		block: func(dst []byte, src string) bool {
			n, err := base64.RawStdEncoding.Decode(dst, []byte(src))
			return err == nil && n == 3
		},
		partial: func(src string) ([]byte, bool) {
			if len(src) != 2 && len(src) != 3 {
				return nil, false
			}
			b, err := base64.RawStdEncoding.DecodeString(src)
			return b, err == nil
		},
	}
	b32Raw   = base32.StdEncoding.WithPadding(base32.NoPadding)
	b32Codec = blockCodec{
		chars: 8, bytes: 5, minBlocks: 2,
		block: func(dst []byte, src string) bool {
			n, err := b32Raw.Decode(dst, []byte(src))
			return err == nil && n == 5
		},
	}
	hexCodec = blockCodec{
		chars: 2, bytes: 1, minBlocks: 6,
		block: func(dst []byte, src string) bool {
			dst[0] = unhex(src[0])<<4 | unhex(src[1])
			return true
		},
	}
)

// resync decodes stream at every alignment, keeps the stretches of whole blocks that decode to
// ASCII text (or UTF-16 of it), and joins them in stream order, so two encoded parts at
// different alignments in one run ("<b1>Q<b2>") decode as one text.
func resync(stream string, c blockCodec) []byte {
	type seg struct {
		start, end int
		data       []byte
	}
	var segs []seg
	buf := make([]byte, c.bytes)
	for off := range c.chars {
		cur := seg{start: -1}
		n := 0
		flush := func() {
			if n >= c.minBlocks {
				segs = append(segs, cur)
			}
			cur, n = seg{start: -1}, 0
		}
		for p := off; p+c.chars <= len(stream); p += c.chars {
			if c.block(buf, stream[p:p+c.chars]) && asciiText(buf) {
				if n == 0 {
					cur.start = p
				}
				cur.data = append(cur.data, buf...)
				cur.end = p + c.chars
				n++
				continue
			}
			flush()
		}
		flush()
	}
	slices.SortStableFunc(segs, func(a, b seg) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})
	var out []byte
	end := 0
	for _, s := range segs {
		if s.end <= end {
			continue
		}
		if s.start < end {
			drop := (end - s.start + c.chars - 1) / c.chars
			s.data = s.data[drop*c.bytes:]
			s.start += drop * c.chars
		} else if gap := s.start - end; end > 0 && c.partial != nil && gap > 0 && gap < c.chars {
			// the short last block of the previous part, cut off by the next part's alignment
			if p, ok := c.partial(stream[end:s.start]); ok && asciiText(p) {
				out = append(out, p...)
			}
		}
		out = append(out, s.data...)
		end = s.end
	}
	if end > 0 && c.partial != nil && len(stream)-end > 0 && len(stream)-end < c.chars {
		if p, ok := c.partial(stream[end:]); ok && asciiText(p) {
			out = append(out, p...)
		}
	}
	return out
}

// decodeBase64 returns the text with each base64 run (both alphabets, padding optional)
// replaced by its best decoding: the run at each of the 4 alignments, or resynchronised.
func decodeBase64(s string) []string {
	runs := findRuns(s, isBase64Char, minRun)
	return replaceRuns(s, runs, func(run string) (string, bool) {
		buf := []byte(run)
		for i, c := range buf {
			switch c {
			case '-':
				buf[i] = '+'
			case '_':
				buf[i] = '/'
			}
		}
		std := string(buf)
		var cands []string
		for off := range 4 {
			r := std[off:]
			if len(r)%4 == 1 {
				r = r[:len(r)-1]
			}
			if len(r) < minRun {
				continue
			}
			if dec, err := base64.RawStdEncoding.DecodeString(r); err == nil {
				if t, ok := asText(dec); ok {
					cands = append(cands, t)
				}
			}
		}
		if t, ok := asText(resync(std, b64Codec)); ok {
			cands = append(cands, t)
		}
		return best(cands)
	})
}

// decodeBase32 returns the text with each base32 run replaced by its best decoding.
func decodeBase32(s string) []string {
	runs := findRuns(s, isBase32Char, minRun)
	return replaceRuns(s, runs, func(run string) (string, bool) {
		up := strings.ToUpper(run)
		var cands []string
		if r := up[:len(up)/8*8]; len(r) >= minRun {
			if dec, err := b32Raw.DecodeString(r); err == nil {
				if t, ok := asText(dec); ok {
					cands = append(cands, t)
				}
			}
		}
		if t, ok := asText(resync(up, b32Codec)); ok {
			cands = append(cands, t)
		}
		return best(cands)
	})
}

// decodeHex returns the text with each hex sequence replaced by its best decoding. A sequence is
// hex digits in groups of any length, joined by up to two separators (":", " ", ",", "-" and the
// like) or written as \xHH or 0xHH, so "6b:6f", "6b6f 696e" and "\x6b\x6f" all decode.
func decodeHex(s string) []string {
	seqs := hexSequences(s)
	if len(seqs) == 0 {
		return nil
	}
	var b strings.Builder
	last := 0
	changed := false
	for _, q := range seqs {
		var cands []string
		for off := range 2 {
			d := q.digits[off:]
			d = d[:len(d)/2*2]
			if len(d) < minRun {
				continue
			}
			dec := make([]byte, len(d)/2)
			for i := range dec {
				dec[i] = unhex(d[2*i])<<4 | unhex(d[2*i+1])
			}
			if t, ok := asText(dec); ok {
				cands = append(cands, t)
			}
		}
		if t, ok := asText(resync(q.digits, hexCodec)); ok {
			cands = append(cands, t)
		}
		t, ok := best(cands)
		if !ok {
			continue
		}
		if !changed {
			b.Grow(len(s))
			changed = true
		}
		b.WriteString(s[last:q.start])
		b.WriteString(t)
		last = q.end
	}
	if !changed {
		return nil
	}
	b.WriteString(s[last:])
	return []string{b.String()}
}

type hexSeq struct {
	start, end int
	digits     string
}

// hexSequences finds the hex sequences of s with at least minRun digits.
func hexSequences(s string) []hexSeq {
	var out []hexSeq
	for i := 0; i < len(s); {
		j := i
		if p := hexPrefix(s, j); p > 0 {
			j += p
		}
		if j >= len(s) || !isHex(s[j]) {
			i++
			continue
		}
		var digits []byte
		start, end := i, i
		for {
			k := j
			for k < len(s) && isHex(s[k]) {
				k++
			}
			if k == j {
				break
			}
			digits = append(digits, s[j:k]...)
			end = k
			// up to two separators, then an optional prefix, then more digits
			m := k
			for n := 0; n < 2 && m < len(s) && isHexSep(s[m]); n++ {
				m++
			}
			m += hexPrefix(s, m)
			if m == k && (k >= len(s) || !isHex(s[k])) {
				break
			}
			j = m
		}
		if len(digits) >= minRun {
			out = append(out, hexSeq{start, end, string(digits)})
		}
		if end > i {
			i = end
		} else {
			i++
		}
	}
	return out
}

func hexPrefix(s string, i int) int {
	if i+2 < len(s) && (s[i] == '\\' || s[i] == '0') && (s[i+1] == 'x' || s[i+1] == 'X') && isHex(s[i+2]) {
		return 2
	}
	return 0
}

func isHexSep(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ':', ',', ';', '.', '-', '_', '|':
		return true
	}
	return false
}

// replaceRuns replaces each run that dec turns into text, and returns the result when any did.
func replaceRuns(s string, runs [][2]int, dec func(string) (string, bool)) []string {
	var b strings.Builder
	last := 0
	changed := false
	for _, r := range runs {
		t, ok := dec(s[r[0]:r[1]])
		if !ok {
			continue
		}
		if !changed {
			b.Grow(len(s))
			changed = true
		}
		b.WriteString(s[last:r[0]])
		b.WriteString(t)
		last = r[1]
	}
	if !changed {
		return nil
	}
	b.WriteString(s[last:])
	return []string{b.String()}
}

// best returns the candidate with the most printable characters, each unprintable one
// counting against it eight times: the decoding at the right alignment.
func best(cands []string) (string, bool) {
	bi, bn := -1, 0
	for i, c := range cands {
		n := 0
		for _, r := range c {
			if r != utf8.RuneError && (r == '\n' || r == '\r' || r == '\t' || unicode.IsPrint(r)) {
				n++
			} else {
				n -= 8
			}
		}
		if bi < 0 || n > bn {
			bi, bn = i, n
		}
	}
	if bi < 0 {
		return "", false
	}
	return cands[bi], true
}

// findRuns returns the [start, end) byte ranges of the maximal runs of in-class bytes at least
// minLen long.
func findRuns(s string, in func(byte) bool, minLen int) [][2]int {
	var runs [][2]int
	start := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && in(s[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= minLen {
			runs = append(runs, [2]int{start, i})
		}
		start = -1
	}
	return runs
}

// asText returns b as text when it is text (see textual), decoding UTF-16 (either byte order,
// either alignment) when its NUL bytes say it is UTF-16, as PowerShell's -EncodedCommand is.
func asText(b []byte) (string, bool) {
	if len(b) >= 2*textualRun && strings.Count(string(b), "\x00")*4 >= len(b) {
		var cands []string
		for _, le := range []bool{true, false} {
			for off := range 2 {
				if u := fromUTF16(b[off:], le); textual([]byte(u)) {
					cands = append(cands, u)
				}
			}
		}
		if t, ok := best(cands); ok {
			return t, true
		}
	}
	if textual(b) {
		return string(b), true
	}
	return "", false
}

func fromUTF16(b []byte, le bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		if le {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	if len(u) > 0 && u[0] == 0xfeff {
		u = u[1:]
	}
	return string(utf16.Decode(u))
}

// textual reports whether b holds text: textualRun printable characters in a row. A misaligned
// or garbage prefix does not hide text after it; random bytes almost never qualify.
func textual(b []byte) bool {
	run := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if r != utf8.RuneError && (r == '\n' || r == '\r' || r == '\t' || unicode.IsPrint(r)) {
			if run++; run >= textualRun {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// asciiText reports whether every byte of a decoded block is printable ASCII, whitespace or a
// NUL (UTF-16 of ASCII).
func asciiText(b []byte) bool {
	for _, c := range b {
		if (c < 0x20 || c >= 0x7f) && c != '\n' && c != '\r' && c != '\t' && c != 0 {
			return false
		}
	}
	return true
}

func isBase64Char(c byte) bool {
	return isAlnumByte(c) || c == '+' || c == '/' || c == '-' || c == '_'
}

func isBase32Char(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '2' <= c && c <= '7'
}

func isAlnumByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func allHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
