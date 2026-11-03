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
	"sync"
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
)

type decoder struct {
	name string
	fn   func(string) []string
}

var decoders = []decoder{
	{name: "percent", fn: decodePercent},
	{name: "escape", fn: decodeEscapes},
	{name: "base64", fn: decodeBase64},
	{name: "base32", fn: decodeBase32},
	{name: "hex", fn: decodeHex},
	{name: "projected", fn: decodeProjected},
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
			if v, n := backslashEscape(s, i); n > 0 {
				i += n - 1
				if n == 4 { // \xHH is a byte
					b.WriteByte(byte(v & 0xff))
					continue
				}
				// a high surrogate followed by a \u low surrogate is one character
				if utf16.IsSurrogate(v) && v < 0xdc00 {
					if lo, m := backslashEscape(s, i+1); m == 6 && lo >= 0xdc00 && lo < 0xe000 {
						v = utf16.DecodeRune(v, lo)
						i += m
					}
				}
				b.WriteRune(v)
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// backslashEscape reads the \xHH, \uHHHH or \UHHHHHHHH escape at s[i], returning its value and
// length (0 when there is none).
func backslashEscape(s string, i int) (rune, int) {
	if i+1 >= len(s) || s[i] != '\\' {
		return 0, 0
	}
	n := 0
	switch s[i+1] {
	case 'x', 'X':
		n = 2
	case 'u':
		n = 4
	case 'U':
		n = 8
	}
	if n == 0 || i+2+n > len(s) || !allHex(s[i+2:i+2+n]) {
		return 0, 0
	}
	var v rune
	for j := range n {
		v = v<<4 | rune(unhex(s[i+2+j]))
	}
	return v, 2 + n
}

// A projection is the text with every byte outside one encoding's alphabet removed. Separators
// between the chunks of an encoded stream are outside the alphabet (an attacker picks them, so
// no list of separators is complete), and the projection reads the stream whole whatever they
// are. Prose around it is mostly inside the base64 and base32 alphabets and stays: resync, which
// keeps only stretches that decode to text, discards what it turns into.
type projection struct {
	name string
	keep [256]bool
}

var projections = []*projection{
	newProjection("base64", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"),
	newProjection("base64url", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"),
	newProjection("base32", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz234567"),
	newProjection("hex", "0123456789abcdefABCDEF"),
}

func newProjection(name, alphabet string) *projection {
	p := &projection{name: name}
	for i := range len(alphabet) {
		p.keep[alphabet[i]] = true
	}
	return p
}

// minSegmentBytes is the shortest stretch resync keeps, and wordy what it must look like, so
// the blocks of prose or binary that decode to ASCII by chance are dropped. A leak worth
// catching is at least the threshold (24 canonical characters) long.
const minSegmentBytes = 16

// minShortBytes is the shortest stretch resync considers as part of a chain (see resync).
const minShortBytes = 5

// decodeProjected decodes the projections of s (see projection). It returns nothing for a text
// that is its own projection (the run decoders cover it) or whose projection is too short.
func decodeProjected(s string) []string {
	var out []string
	buf := make([]byte, 0, len(s))
	for _, p := range projections {
		buf = buf[:0]
		for i := 0; i < len(s); i++ {
			c := s[i]
			// the x of a 0x or \x prefix is outside the hex alphabet; the 0 is not
			if p.name == "hex" && c == '0' && i+1 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') {
				i++
				continue
			}
			if p.keep[c] {
				buf = append(buf, c)
			}
		}
		if len(buf) < minRun || len(buf) == len(s) {
			continue
		}
		var cands []string
		switch p.name {
		case "base64", "base64url":
			for i, c := range buf {
				switch c {
				case '-':
					buf[i] = '+'
				case '_':
					buf[i] = '/'
				}
			}
			cands = b64Candidates(buf)
		case "base32":
			for i, c := range buf {
				if 'a' <= c && c <= 'z' {
					buf[i] = c - 'a' + 'A'
				}
			}
			cands = b32Candidates(buf)
		default:
			cands = hexCandidates(string(buf))
		}
		if t, ok := best(cands); ok {
			out = append(out, t)
		}
	}
	return out
}

// wordy reports whether at least 60% of the bytes of b are letters, digits or spaces, as text is
// and the chance ASCII of a misaligned decoding is not. NULs (UTF-16) and ASCII punctuation are
// neutral: an attacker can put one punctuation character between the letters ("t-h-e- -s-e"),
// which halves their share, so punctuation is left out of the count and a floor of
// minSegmentBytes/2 letters, digits and spaces stands in for the length it no longer adds.
func wordy(b []byte) bool {
	n, w := 0, 0
	for _, c := range b {
		switch {
		case c == 0 || isASCIIPunct(c):
		case isAlnumByte(c) || c == ' ':
			n++
			w++
		default:
			n++
		}
	}
	return w >= minSegmentBytes/2 && 5*w >= 3*n
}

func isASCIIPunct(c byte) bool {
	return 0x21 <= c && c <= 0x7e && !isAlnumByte(c)
}

// A blockCodec decodes one fixed-size block of an encoding; resync uses it to decode a stream
// at whatever alignment each part of it has.
type blockCodec struct {
	chars, bytes int
	table        [256]int8 // symbol value, -1 outside the alphabet
	bits         uint
}

func newCodec(alphabet string, chars, bytes int, bits uint, fold bool) blockCodec {
	c := blockCodec{chars: chars, bytes: bytes, bits: bits}
	for i := range c.table {
		c.table[i] = -1
	}
	for i := range len(alphabet) {
		c.table[alphabet[i]] = int8(i)
		if fold && 'A' <= alphabet[i] && alphabet[i] <= 'Z' {
			c.table[alphabet[i]+'a'-'A'] = int8(i)
		}
	}
	return c
}

var (
	b64StdCodec = newCodec("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 4, 3, 6, false)
	b32Codec    = newCodec("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 8, 5, 5, true)
	b32Raw      = base32.StdEncoding.WithPadding(base32.NoPadding)
	hexCodec    = newCodec("0123456789abcdef", 2, 1, 4, true)
)

// block decodes src (chars symbols, or fewer for a short last block) into dst and returns the
// number of whole bytes, or -1 when a symbol is outside the alphabet.
func (c *blockCodec) block(dst, src []byte) int {
	var v uint64
	for _, s := range src {
		t := c.table[s]
		if t < 0 {
			return -1
		}
		v = v<<c.bits | uint64(t)
	}
	nbits := uint(len(src)) * c.bits
	n := int(nbits / 8)
	v >>= nbits - uint(n)*8
	for i := n - 1; i >= 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
	return n
}

// textBlock reports whether the whole block src decodes to ASCII text (see asciiText) without
// writing it anywhere.
func (c *blockCodec) textBlock(src []byte) bool {
	var v uint64
	for _, s := range src {
		t := c.table[s]
		if t < 0 {
			return false
		}
		v = v<<c.bits | uint64(t)
	}
	for range c.bytes {
		if !asciiOK[byte(v)] {
			return false
		}
		v >>= 8
	}
	return true
}

// asciiOK holds the bytes asciiText accepts.
var asciiOK = func() (t [256]bool) {
	for c := 0x20; c < 0x7f; c++ {
		t[c] = true
	}
	t['\n'], t['\r'], t['\t'], t[0] = true, true, true, true
	return t
}()

// resync decodes stream at every alignment, keeps the stretches that decode to wordy ASCII text
// (or UTF-16 of it), and joins them in stream order, so two encoded parts at different
// alignments in one run ("<b1>Q<b2>") decode as one text. A stretch is kept when it is at least
// minSegmentBytes long. Shorter ones (down to minShortBytes, base64 and base32 only) are kept
// when they chain: they touch (within one block) the stretches beside them, as the pieces of
// text encoded one by one and joined do, and the chain is together long and wordy enough.
func resync(stream []byte, c *blockCodec) []byte {
	minBlocks := (minSegmentBytes + c.bytes - 1) / c.bytes
	shortBlocks := minBlocks // 1 block: no short stretches
	if c.chars > 2 {
		shortBlocks = (minShortBytes + c.bytes - 1) / c.bytes
	}
	type seg struct {
		start, end int
		data       []byte
		long       bool
	}
	var segs, shorts []seg
	buf := make([]byte, c.bytes)
	for off := range c.chars {
		start, n := -1, 0
		flush := func(end int) {
			if n >= shortBlocks {
				data := make([]byte, 0, n*c.bytes)
				for p := start; p < end; p += c.chars {
					c.block(buf, stream[p:p+c.chars])
					data = append(data, buf...)
				}
				switch {
				case n < minBlocks:
					shorts = append(shorts, seg{start, end, data, false})
				case wordy(data):
					segs = append(segs, seg{start, end, data, true})
				}
			}
			start, n = -1, 0
		}
		p := off
		for ; p+c.chars <= len(stream); p += c.chars {
			if c.textBlock(stream[p : p+c.chars]) {
				if n == 0 {
					start = p
				}
				n++
				continue
			}
			flush(p)
		}
		flush(p)
	}
	byStart := func(a, b seg) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	}
	slices.SortStableFunc(segs, byStart)
	var parts []seg // the stretches in stream order, each cut to start where the one before ends
	end := 0
	for _, s := range segs {
		if s.end <= end {
			continue
		}
		if s.start < end {
			drop := (end - s.start + c.chars - 1) / c.chars
			s.data = s.data[drop*c.bytes:]
			s.start += drop * c.chars
		}
		parts = append(parts, s)
		end = s.end
	}
	if len(shorts) > 0 {
		// a short stretch joins only where it overlaps none of the others
		slices.SortStableFunc(shorts, byStart)
		var all []seg
		pi, last := 0, 0
		for _, s := range shorts {
			if s.start < last {
				continue
			}
			for pi < len(parts) && parts[pi].end <= s.start {
				all = append(all, parts[pi])
				pi++
			}
			if pi < len(parts) && parts[pi].start < s.end {
				continue
			}
			all = append(all, s)
			last = s.end
		}
		parts = append(all, parts[pi:]...)
		slices.SortStableFunc(parts, byStart)
	}
	var out []byte
	partial := func(src []byte) []byte {
		if len(src) == 0 || len(src) >= c.chars {
			return nil
		}
		if n := c.block(buf, src); n > 0 && asciiText(buf[:n]) {
			return buf[:n]
		}
		return nil
	}
	// chains: parts at most one block apart (the short last block of one part is cut off by the
	// next part's alignment and sits in that gap)
	for i := 0; i < len(parts); {
		j := i + 1
		for j < len(parts) && parts[j].start-parts[j-1].end < c.chars {
			j++
		}
		var chain []byte
		long := false
		for k := i; k < j; k++ {
			if k > i {
				chain = append(chain, partial(stream[parts[k-1].end:parts[k].start])...)
			}
			chain = append(chain, parts[k].data...)
			long = long || parts[k].long
		}
		if long || len(chain) >= minSegmentBytes && wordy(chain) {
			out = append(out, chain...)
			if j == len(parts) {
				out = append(out, partial(stream[parts[j-1].end:])...)
			}
		}
		i = j
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
		return best(b64Candidates(buf))
	})
}

// b64Candidates are the text readings of buf, a stream of the standard base64 alphabet: at each
// of its 4 alignments, and resynchronised.
func b64Candidates(buf []byte) []string {
	var cands []string
	dec := make([]byte, base64.RawStdEncoding.DecodedLen(len(buf)))
	for off := range min(4, len(buf)+1) {
		r := buf[off:]
		if len(r)%4 == 1 {
			r = r[:len(r)-1]
		}
		if len(r) < minRun {
			continue
		}
		if n, err := base64.RawStdEncoding.Decode(dec, r); err == nil {
			if t, ok := asText(dec[:n]); ok {
				cands = append(cands, t)
			}
		}
	}
	if t, ok := resyncText(resync(buf, &b64StdCodec)); ok {
		cands = append(cands, t)
	}
	return cands
}

// decodeBase32 returns the text with each base32 run replaced by its best decoding.
func decodeBase32(s string) []string {
	runs := findRuns(s, isBase32Char, minRun)
	return replaceRuns(s, runs, func(run string) (string, bool) {
		up := []byte(run)
		for i, c := range up {
			if 'a' <= c && c <= 'z' {
				up[i] = c - 'a' + 'A'
			}
		}
		return best(b32Candidates(up))
	})
}

// b32Candidates are the text readings of up, a stream of the upper-case base32 alphabet.
func b32Candidates(up []byte) []string {
	var cands []string
	if r := up[:len(up)/8*8]; len(r) >= minRun {
		dec := make([]byte, b32Raw.DecodedLen(len(r)))
		if n, err := b32Raw.Decode(dec, r); err == nil {
			if t, ok := asText(dec[:n]); ok {
				cands = append(cands, t)
			}
		}
	}
	if t, ok := resyncText(resync(up, &b32Codec)); ok {
		cands = append(cands, t)
	}
	return cands
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
		t, ok := best(hexCandidates(q.digits))
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

// hexCandidates are the text readings of digits, a string of hex digits, at both alignments and
// resynchronised.
func hexCandidates(digits string) []string {
	var cands []string
	for off := range min(2, len(digits)+1) {
		d := digits[off:]
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
	if t, ok := resyncText(resync([]byte(digits), &hexCodec)); ok {
		cands = append(cands, t)
	}
	return cands
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

// best returns the candidate with the highest score: a character that folds to an ASCII letter
// or digit in the matching form (see Normalise) scores 2, another printable character 1, and an
// unprintable one -8. The decoding at the right alignment and in the right byte order wins: a
// misread one is either unprintable or text of no script in particular.
func best(cands []string) (string, bool) {
	bi, bn := -1, 0
	var buf [16]rune
	for i, c := range cands {
		n := 0
		for _, r := range c {
			switch {
			case r == utf8.RuneError || r != '\n' && r != '\r' && r != '\t' && !unicode.IsPrint(r) && !unicode.IsSpace(r):
				n -= 8
			case r < utf8.RuneSelf && unicode.IsLetter(r) || unicode.IsDigit(r) && r < utf8.RuneSelf:
				n += 2
			case r >= utf8.RuneSelf && foldsToASCII(appendCanonRune(buf[:0], r, 0)):
				n += 2
			default:
				n++
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

func foldsToASCII(rs []rune) bool {
	return len(rs) > 0 && rs[0] < utf8.RuneSelf
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

// asText returns b as text when it is text (see textual). Its UTF-16 readings (either byte
// order, either alignment) are always candidates too, as PowerShell's -EncodedCommand is
// UTF-16LE base64; a reading counts only when its code units sit in at most two 256-character
// blocks for three quarters of the text, as one script's text does and random bytes do not.
func asText(b []byte) (string, bool) { return asTextWith(b, textual) }

// resyncText is asText for the output of resync, whose segments were already judged to be text
// (see wordy): it needs only a printable run, so NUL padding an attacker puts before the text
// (which a straight decoding fails on) does not hide it.
func resyncText(b []byte) (string, bool) {
	return asTextWith(b, func(b []byte) bool { return longestPrintable(b) >= textualRun })
}

func asTextWith(b []byte, isText func([]byte) bool) (string, bool) {
	if len(b) < minSegmentBytes {
		return "", false
	}
	var cands []string
	if isText(b) {
		cands = append(cands, string(b))
	}
	if len(b) >= 2*textualRun {
		for _, le := range []bool{true, false} {
			for off := range 2 {
				if u, ok := fromUTF16(b[off:], le); ok && textual([]byte(u)) {
					cands = append(cands, u)
				}
			}
		}
	}
	return best(cands)
}

// bmpNeutral marks the code units of the Basic Multilingual Plane that are spaces or punctuation.
var bmpNeutral = sync.OnceValue(func() *[1 << 16]bool {
	var t [1 << 16]bool
	for u := range t {
		r := rune(u)
		t[u] = unicode.IsSpace(r) || unicode.IsPunct(r)
	}
	return &t
})

func fromUTF16(b []byte, le bool) (string, bool) {
	unit := func(i int) uint16 {
		if le {
			return uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	n := len(b) / 2
	// one script's text sits in at most two 256-character blocks (its own and ASCII). What an
	// attacker puts between the letters is left out of the count: spaces and punctuation (U+3000,
	// U+2014) and pairs that are no letter or digit (emoji), which sit in blocks of their own.
	// What is left (letters, digits, marks, other symbols, controls) must be a quarter of the
	// characters and sit in two blocks for three quarters of it.
	var pages [256]int
	counted, chars := 0, 0
	neutral := bmpNeutral()
	for i := 0; i < n; i++ {
		u := unit(i)
		chars++
		if 0xd800 <= u && u < 0xdc00 && i+1 < n { // a surrogate pair is one character
			r := utf16.DecodeRune(rune(u), rune(unit(i+1)))
			i++
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				continue
			}
		} else if neutral[u] {
			continue
		}
		pages[u>>8]++
		counted++
	}
	first, second := 0, 0
	for _, c := range pages {
		if c > first {
			first, second = c, first
		} else if c > second {
			second = c
		}
	}
	if 4*counted < chars || 4*(first+second) < 3*counted {
		return "", false
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = unit(i)
	}
	if len(u) > 0 && u[0] == 0xfeff {
		u = u[1:]
	}
	return string(utf16.Decode(u)), true
}

// textual reports whether b holds text: textualRun printable characters in a row, at least 90%
// of it printable, and at least half of it letters, digits and spaces. Random bytes, which by
// chance hold such a run once every 150 KB or so, fail the shares; a decoding with a garbage
// prefix or a part at another alignment fails them too, and resync recovers its text.
//
// Punctuation and symbols are neutral in the letters share (an attacker can put one between
// every two letters, "t-h-e- -s-e-c-r-e-t", or an emoji), and any Unicode space is a space
// (U+3000 between Cyrillic letters); the letters, digits and spaces must still be at least
// textualRun/2 characters and half of the rest.
func textual(b []byte) bool {
	run, longest, printable, wordish, neutral, total := 0, 0, 0, 0, 0, 0
	for i := 0; i < len(b); {
		if c := b[i]; c < utf8.RuneSelf { // ASCII, without the unicode tables
			i++
			total++
			switch {
			case isAlnumByte(c) || c == ' ' || c == '\n' || c == '\r' || c == '\t':
				printable++
				wordish++
			case 0x21 <= c && c <= 0x7e:
				printable++
				neutral++
			default:
				run = 0
				if 10*(total-printable) > len(b) { // over 10% unprintable already
					return false
				}
				continue
			}
			run++
			longest = max(longest, run)
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		total++
		if r != utf8.RuneError && (unicode.IsPrint(r) || unicode.IsSpace(r)) {
			printable++
			switch {
			case unicode.IsSpace(r) || unicode.IsLetter(r) || unicode.IsDigit(r):
				wordish++
			case unicode.IsPunct(r) || unicode.IsSymbol(r):
				neutral++
			}
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
		if 10*(total-printable) > len(b) {
			return false
		}
	}
	return longest >= textualRun && 10*printable >= 9*total && wordish >= textualRun/2 && 2*wordish >= total-neutral
}

// longestPrintable is the longest stretch of printable ASCII in b. NULs neither extend nor end
// it: UTF-16 of ASCII has one between every two characters, and an attacker may put one every
// few bytes.
func longestPrintable(b []byte) int {
	run, longest := 0, 0
	for _, c := range b {
		switch {
		case c == 0:
		case c >= 0x20 && c < 0x7f || c == '\n' || c == '\r' || c == '\t':
			run++
			longest = max(longest, run)
		default:
			run = 0
		}
	}
	return longest
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
