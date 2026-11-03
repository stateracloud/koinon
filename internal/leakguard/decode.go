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
	"sort"
	"strings"
	"sync"
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

// parallelText is the length from which the decoders of a text run side by side: they are
// independent, and a megabyte takes each of them a good part of a second.
const parallelText = 128 << 10

// runDecoders runs every decoder on s and returns their children, in the order of decoders.
func runDecoders(s string) [][]string {
	kids := make([][]string, len(decoders))
	if len(s) < parallelText {
		for i, d := range decoders {
			kids[i] = d.fn(s)
		}
		return kids
	}
	var wg sync.WaitGroup
	for i, d := range decoders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kids[i] = d.fn(s)
		}()
	}
	wg.Wait()
	return kids
}

// minCovered is the length from which a decoding that is part of another variant is dropped.
const minCovered = 64

// coveredBy reports whether child is a part of one of the variants: it holds no k-gram they do
// not, and decoding it again finds what decoding them finds. The projection of a stream, which
// keeps the edges of a decoding off (a base64 run and its projection are one text but for a
// few bytes at the ends), is such a part of the base64 reading, and decoding it costs as much.
func coveredBy(vs []Variant, child string) bool {
	if len(child) < minCovered {
		return false
	}
	for _, v := range vs {
		if len(v.Text) > len(child) && strings.Contains(v.Text, child) {
			return true
		}
	}
	return false
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
		// Each decoder makes a pass over the text, most of them a cheap scan that finds nothing to
		// decode: the text is charged twice for the six, and the clock is the bound that counts.
		if err := m.add(2 * len(it.v.Text)); err != nil {
			return nil, err
		}
		kids := runDecoders(it.v.Text)
		for di, d := range decoders {
			for _, child := range kids[di] {
				if _, dup := seen[child]; dup {
					continue
				}
				if coveredBy(out, child) {
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

// minWideUnits is the fewest characters of a UTF-16 reading: a leak is at least the threshold (24
// canonical characters) long, and a shorter random reading looks like text by chance too often.
const minWideUnits = 24

// minShortBytes is the shortest stretch resync considers as part of a chain (see resync).
const minShortBytes = 5

// decodeProjected decodes the projections of s (see projection). It returns nothing for a text
// that is its own projection (the run decoders cover it) or whose projection is too short.
func decodeProjected(s string) []string {
	var out []string
	var rs [][]string
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
		var cands readings
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
		rs = append(rs, cands.all())
	}
	// the k-th reading of each projection makes the k-th child
	for k := 0; ; k++ {
		more := false
		for _, r := range rs {
			if k < len(r) {
				more = true
				if !slices.Contains(out, r[k]) {
					out = append(out, r[k])
				}
			}
		}
		if !more {
			return out
		}
	}
}

// wordy reports whether at least 60% of the bytes of b are letters, digits or spaces, as text is
// and the chance ASCII of a misaligned decoding is not. Control bytes (the NULs of UTF-16, a \x01
// filler) and ASCII punctuation are neutral: an attacker can put one punctuation character between the letters ("t-h-e- -s-e"),
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
	rs := make([][]string, len(runs))
	for i, r := range runs {
		if allHex(s[r[0]:r[1]]) { // a hash or an id: its base64 reading is bytes that look like ideographs
			continue
		}
		buf := []byte(s[r[0]:r[1]])
		for i, c := range buf {
			switch c {
			case '-':
				buf[i] = '+'
			case '_':
				buf[i] = '/'
			}
		}
		rs[i] = b64Candidates(buf).all()
	}
	return replaceSpans(s, runs, rs)
}

// b64Candidates are the text readings of buf, a stream of the standard base64 alphabet: at each
// of its 4 alignments, and resynchronised.
func b64Candidates(buf []byte) readings { return b64Readings(buf, true) }

// b64Readings is b64Candidates, with the resynchronised reading only when sync.
func b64Readings(buf []byte, sync bool) readings {
	dec := make([]byte, base64.RawStdEncoding.DecodedLen(len(buf)))
	var cands, noisy readings
	for off := range min(4, len(buf)+1) {
		r := buf[off:]
		if len(r)%4 == 1 {
			r = r[:len(r)-1]
		}
		if len(r) < minRun {
			continue
		}
		if n, err := base64.RawStdEncoding.Decode(dec, r); err == nil {
			cands.add(asTextOrNoisy(dec[:n], sync, &noisy))
		}
	}
	return finish(cands, noisy, sync, func() readings { return resyncText(resync(buf, &b64StdCodec)) })
}

// decodeBase32 returns the text with each base32 run replaced by its best decoding.
func decodeBase32(s string) []string {
	runs := findRuns(s, isBase32Char, minRun)
	rs := make([][]string, len(runs))
	for i, r := range runs {
		up := []byte(s[r[0]:r[1]])
		for i, c := range up {
			if 'a' <= c && c <= 'z' {
				up[i] = c - 'a' + 'A'
			}
		}
		rs[i] = b32Candidates(up).all()
	}
	return replaceSpans(s, runs, rs)
}

// b32Candidates are the text readings of up, a stream of the upper-case base32 alphabet.
func b32Candidates(up []byte) readings { return b32Readings(up, true) }

// b32Readings is b32Candidates, with the resynchronised reading only when sync.
func b32Readings(up []byte, sync bool) readings {
	var cands, noisy readings
	if r := up[:len(up)/8*8]; len(r) >= minRun {
		dec := make([]byte, b32Raw.DecodedLen(len(r)))
		if n, err := b32Raw.Decode(dec, r); err == nil {
			cands.add(asTextOrNoisy(dec[:n], sync, &noisy))
		}
	}
	return finish(cands, noisy, sync, func() readings { return resyncText(resync(up, &b32Codec)) })
}

// decodeHex returns the text with each hex sequence replaced by its best decoding. A sequence is
// hex digits in groups of any length, joined by up to two separators (":", " ", ",", "-" and the
// like) or written as \xHH or 0xHH, so "6b:6f", "6b6f 696e" and "\x6b\x6f" all decode.
func decodeHex(s string) []string {
	seqs := hexSequences(s)
	if len(seqs) == 0 {
		return nil
	}
	spans := make([][2]int, len(seqs))
	rs := make([][]string, len(seqs))
	for i, q := range seqs {
		spans[i] = [2]int{q.start, q.end}
		rs[i] = hexCandidates(q.digits).all()
	}
	return replaceSpans(s, spans, rs)
}

// hexCandidates are the text readings of digits, a string of hex digits, at both alignments and
// resynchronised.
func hexCandidates(digits string) readings { return hexReadings(digits, true) }

// hexReadings is hexCandidates, with the resynchronised reading only when sync.
func hexReadings(digits string, sync bool) readings {
	var cands, noisy readings
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
		cands.add(asTextOrNoisy(dec, sync, &noisy))
	}
	return finish(cands, noisy, sync, func() readings { return resyncText(resync([]byte(digits), &hexCodec)) })
}

// asTextOrNoisy is asTextStrict of one alignment of a stream; when it is no text and sync, its
// reading with invalid bytes stripped (see asTextLax) goes to noisy, which finish uses when no
// alignment is text.
func asTextOrNoisy(b []byte, sync bool, noisy *readings) readings {
	r := asTextStrict(b)
	if sync && r.empty() {
		noisy.add(asTextLax(b))
	}
	return r
}

// finish adds to the readings of the alignments of a stream those of its resynchronised form
// (when sync), and the noisy readings when no alignment was text: it may be text with noise.
func finish(cands, noisy readings, sync bool, resynced func() readings) readings {
	if sync && cands.empty() {
		cands = noisy
	}
	if sync {
		cands.add(resynced())
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

// replaceSpans returns the text with each span replaced by one of its readings: the first child
// by the first reading of each span, the second by the second (or the first, for a span with
// one), and so on. A span without readings stays. It returns nothing when no span has any.
func replaceSpans(s string, spans [][2]int, rs [][]string) []string {
	k := 0
	for _, r := range rs {
		k = max(k, len(r))
	}
	var out []string
	for j := range k {
		var b strings.Builder
		b.Grow(len(s))
		last := 0
		for i, sp := range spans {
			if len(rs[i]) == 0 {
				continue
			}
			b.WriteString(s[last:sp[0]])
			b.WriteString(rs[i][min(j, len(rs[i])-1)])
			last = sp[1]
		}
		b.WriteString(s[last:])
		if c := b.String(); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// A readings holds the texts one encoded stream may be: the readings as 8-bit text (at each
// alignment, and resynchronised) and as UTF-16 (either byte order, either alignment). One
// stream has no single right reading that can be told from the wrong ones by a score: a reading
// misaligned by a byte scores like the real one on some scripts, and an attacker can shape the
// text so that it does. Every reading that passes the loose gates is checked.
type readings struct {
	plain []string
	wide  []string
}

// maxWide is how many UTF-16 readings of one stream are kept.
const maxWide = 4

// maxPlain is how many 8-bit readings of one stream are kept: a stream has 4 alignments and a
// resynchronised reading, and text in UTF-8 of a script of two-byte letters reads as text at two of them.
const maxPlain = 4

func (r *readings) add(o readings) {
	r.plain = append(r.plain, o.plain...)
	r.wide = append(r.wide, o.wide...)
}

// all returns the readings to check: the 8-bit readings that are text, then the UTF-16 readings,
// the highest scoring maxPlain and maxWide of them when there are more.
func (r readings) all() []string {
	var out []string
	for _, p := range top(sound(r.plain), maxPlain) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, w := range top(r.wide, maxWide) {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// maxInvalidShare is the share of bytes (1 in 8) that may be no UTF-8 in an 8-bit reading that is
// kept beside a cleaner one.
const maxInvalidShare = 8

// sound drops from cands the readings that are mostly noise: 1 byte in maxInvalidShare or more is
// no UTF-8, and another reading with less than that holds at least four fifths as many letters.
// A stream read at another bit alignment holds text of a script of two-byte letters as other
// letters with an invalid byte in five, and costs a whole pass of decoders (and the next level
// of decoding) for nothing. A noisy reading is kept when no reading is quiet: an invalid byte is
// noise the sender may put anywhere (Normalise drops it), so its share alone says nothing about
// the text.
func sound(cands []string) []string {
	if len(cands) < 2 {
		return cands
	}
	t := runeClasses()
	letters := make([]int, len(cands))
	bad := make([]int, len(cands))
	clean := 0 // the letters of the reading with little noise that has the most
	for k, c := range cands {
		for i := 0; i < len(c); {
			r, size := utf8.DecodeRuneInString(c[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				bad[k]++
			case classOf(t, r)&clsWord != 0:
				letters[k]++
			}
			i += size
		}
		if bad[k]*maxInvalidShare < len(c) {
			clean = max(clean, letters[k])
		}
	}
	var out []string
	for k, c := range cands {
		if bad[k]*maxInvalidShare < len(c) || 4*letters[k] > 5*clean {
			out = append(out, c)
		}
	}
	return out
}

// top returns cands, or the n highest scoring of them (in their order) when there are more.
func top(cands []string, n int) []string {
	if len(cands) <= n {
		return cands
	}
	sc := make([]int, len(cands))
	idx := make([]int, len(cands))
	for i, c := range cands {
		sc[i], idx[i] = score(c), i
	}
	sort.SliceStable(idx, func(a, b int) bool { return sc[idx[a]] > sc[idx[b]] })
	idx = idx[:n]
	sort.Ints(idx)
	out := make([]string, 0, n)
	for _, i := range idx {
		out = append(out, cands[i])
	}
	return out
}

// score rates a reading: a character that folds to an ASCII letter or digit in the matching
// form (see Normalise) scores 2 (3 when it is ASCII itself), another printable character 1, a
// filler (control, format or combining character) 0, and an unprintable one -8.
func score(c string) int {
	t := runeClasses()
	n := 0
	// A filler (an invalid byte, a control, a format or combining character) scores nothing when
	// it is one of two kinds, as an attacker's is; a reading with more kinds of it is a
	// misreading, and each costs.
	var kinds [3]rune
	nKinds := 0
	for i, r := range c {
		cl := classOf(t, r)
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(c[i:]); size == 1 {
				n -= 8 // an invalid byte is no filler: it costs
				continue
			}
		}
		if r != 0 && cl&clsInvis != 0 {
			if !slices.Contains(kinds[:nKinds], r) && nKinds < len(kinds) {
				kinds[nKinds] = r
				nKinds++
			}
			if slices.Contains(kinds[:min(nKinds, 2)], r) {
				continue
			}
			n -= 8
			continue
		}
		switch {
		case r == utf8.RuneError:
			n -= 8
		case r < utf8.RuneSelf && cl&clsFold != 0:
			n += 3
		case cl&clsFold != 0:
			n += 2
		case cl&clsPrint == 0 && r != '\n' && r != '\r' && r != '\t':
			n -= 8
		case r >= 1<<16:
			n += 6
		default:
			n++
		}
	}
	return n
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

// asTextStrict returns the readings of b that are text: b itself when it is text (see textual), and
// its UTF-16 readings (either byte order, either alignment), which are always candidates too, as
// PowerShell's -EncodedCommand is UTF-16LE base64. A UTF-16 reading counts when it passes the
// gate of utf16Gate and is text itself.
func asTextStrict(b []byte) readings { return asTextWith(b, textual, true) }

// asTextLax is asTextStrict for a stream no alignment of which is text, and without the UTF-16
// readings, which were tried: b is text when invalid bytes of any number of kinds are stripped
// (see textualLax). The short windows of a call of many arguments do not get it: there are
// thousands of them, and at that length a window of another alignment reads as text with an
// invalid byte in five as often as a real one does.
func asTextLax(b []byte) readings { return asTextWith(b, textualLax, false) }

// empty reports whether no reading was found.
func (r readings) empty() bool { return len(r.plain) == 0 && len(r.wide) == 0 }

// resyncText is asText for the output of resync, whose segments were already judged to be text
// (see wordy): it needs only a printable run, so NUL padding an attacker puts before the text
// (which a straight decoding fails on) does not hide it.
func resyncText(b []byte) readings {
	return asTextWith(b, func(b []byte) bool { return longestPrintable(b) >= textualRun }, true)
}

func asTextWith(b []byte, isText func([]byte) bool, wide bool) readings {
	var r readings
	if len(b) < minSegmentBytes {
		return r
	}
	if isText(b) {
		r.plain = append(r.plain, string(b))
	}
	if wide && len(b) >= 2*textualRun {
		for _, le := range []bool{true, false} {
			for off := range 2 {
				if u, ok := fromUTF16(b[off:], le); ok && textual([]byte(u)) {
					r.wide = append(r.wide, u)
				}
			}
		}
	}
	return r
}

// utf16Gate reports whether the code units of b look like one script's text, loosely: a reading
// that passes is checked, one that fails is dropped, and several may pass (see readings). One
// script's text sits in two 256-character blocks at most (its own and ASCII), and Chinese,
// Japanese and Korean in a few more, which are one block each here (ideographs span about eighty
// of 256, Hangul forty-four). What an attacker puts between the letters is left out of the
// count: spaces, punctuation and invisible marks (U+3000, U+2014, U+200B) and pairs that are no
// letter or digit (emoji). What is left must be a quarter of the characters, with the unprintable
// ones (controls, unassigned and private-use characters: a third of random units) under 5%, and sit in two blocks (three when one is ideographs, as Japanese
// is kana, ideographs and ASCII) for three quarters of it. Random bytes put 42% of their units
// among the ideographs and the rest across 190 blocks.
func utf16Gate(b []byte, le bool) bool {
	n := len(b) / 2
	if n < minWideUnits {
		return false
	}
	const han = 256 // bucket of U+4E00 to U+9FFF
	var pages [257]int32
	p1, p2 := 0, 1
	var c1, c2 int32
	var seen [8]uint16 // the first distinct ideographs: eight are enough (see below)
	distinct := 0
	merged := false
	counted, chars, bad, hanUnits, hanWide := 0, 0, 0, 0, 0
	cls := runeClasses()
	for i := 0; i < n; i++ {
		var u uint16
		if le {
			u = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
		chars++
		c := cls[u]
		if 0xd800 <= u && u < 0xdc00 && i+1 < n { // a surrogate pair is one character
			var lo uint16
			if le {
				lo = uint16(b[2*i+2]) | uint16(b[2*i+3])<<8
			} else {
				lo = uint16(b[2*i+2])<<8 | uint16(b[2*i+3])
			}
			i++
			if c := classOf(cls, utf16.DecodeRune(rune(u), rune(lo))); c&clsWord == 0 || c&clsSpace != 0 {
				continue
			}
		} else if c&(clsSpace|clsPunctOnly|clsMark) != 0 {
			continue
		} else if c&clsPrint == 0 {
			bad++           // a control, a lone surrogate, an unassigned or private-use character
			if 20*bad > n { // more than 5% of all the units: the reading fails whatever follows
				return false
			}
		}
		switch {
		default:
			p := int(u >> 8)
			v := pages[p] + 1
			pages[p] = v
			switch { // the two fullest blocks so far
			case p == p1:
				c1 = v
			case v > c1:
				p2, c2, p1, c1 = p1, c1, p, v
			case p == p2:
				c2 = v
			case v > c2:
				p2, c2 = p, v
			}
			if 0x4e00 <= u && u <= 0x9fff {
				hanUnits++
				if u&0x80 != 0 {
					hanWide++
				}
				if distinct < len(seen) && !slices.Contains(seen[:distinct], u) {
					seen[distinct] = u
					distinct++
				}
			}
		}
		counted++
	}
	// Ideographs are one block, unless they are ASCII read in pairs: a pair of printable ASCII
	// bytes lands in 0x2020 to 0x7e7e, and never has a byte of 0x80 or more, as half of the
	// ideographs do.
	// (and not a pattern repeating: a run of one byte value in base64 reads as a few ideographs)
	if hanUnits > 0 && 4*hanWide >= hanUnits && distinct >= 8 {
		for p := 0x4e; p <= 0x9f; p++ {
			pages[p] = 0
		}
		pages[han] = int32(hanUnits)
		merged = true
	}
	if 4*counted < chars || 20*bad > chars {
		return false
	}
	first, second := c1, c2
	if merged {
		first, second = 0, 0
		for _, c := range pages {
			if c > first {
				first, second = c, first
			} else if c > second {
				second = c
			}
		}
	}
	// Japanese is kana, ideographs and ASCII: three blocks
	top := max(first+second, pages[han]+pages[0x30]+pages[0])
	return 4*int(top) >= 3*counted
}

func fromUTF16(b []byte, le bool) (string, bool) {
	if !utf16Gate(b, le) {
		return "", false
	}
	n := len(b) / 2
	u := make([]uint16, n)
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
func textual(b []byte) bool { return textualWith(b, false) }

// textualLax is textual with the invalid bytes of any number of kinds stripped (see stripSound).
func textualLax(b []byte) bool { return textualWith(b, true) }

func textualWith(b []byte, strip bool) bool {
	t := runeClasses()
	run, longest, printable, wordish, punct, total := 0, 0, 0, 0, 0, 0
	var inv [4]rune              // the invisible characters seen (see below)
	nInv, nRaw := 0, 0           // how many there are, and how many of them are controls
	nBad := 0                    // the invalid bytes, stripped
	printBytes := 0              // the bytes of the printable characters
	var pages [pageBuckets]int32 // the letters beyond ASCII, by page
	nonASCII := 0
	var letterSet [64]int32 // how often each letter was seen, hashed to 64 buckets
	for i := 0; i < len(b); {
		if strip && i >= earlyLook && 100*printBytes < earlyPrintablePercent*i {
			return false // an early look (see earlyLook)
		}
		r, size, bad := rune(b[i]), 1, false
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				r = -rune(b[i]) - 1 // an invalid byte
				bad = true
			}
		}
		i += size
		cl := classOf(t, r)
		if cl&clsInvis != 0 {
			// an attacker's filler (U+200B, U+00AD, a combining mark, \x01, \xff after every
			// character) is skipped, but only four kinds of it, and two of control or invalid
			// bytes: random bytes hold dozens of both
			seen := false
			for _, v := range inv[:nInv] {
				seen = seen || v == r
			}
			raw := cl&clsMark == 0
			if !seen && nInv < len(inv) && (!raw || nRaw < 2) {
				inv[nInv] = r
				nInv++
				if raw {
					nRaw++
				}
				seen = true
			}
			if seen {
				printBytes += size
				continue
			}
			if bad && strip {
				// Past the two kinds of filler, an invalid byte is still noise the sender
				// controls (Normalise drops it): it is stripped, up to a share of the block.
				nBad++
				if 100*nBad > maxStrippedPercent*len(b) {
					return false
				}
				continue
			}
		}
		total++
		if cl&clsPrint != 0 {
			printable++
			printBytes += size
			switch {
			case cl&clsWord != 0:
				wordish++
				if strip {
					letterSet[int(r)*0x9e3779b1>>26&63]++
					if r >= utf8.RuneSelf {
						pages[pageOf(r)]++
						nonASCII++
					}
				}
			case cl&clsPunct != 0:
				punct++
			}
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
		if 10*(total-printable) > len(b) { // over 10% unprintable already
			return false
		}
	}
	return (nBad == 0 || stripSound(nBad, len(b), printBytes, wordish, total) && oneScript(&pages, nonASCII) && variety(&letterSet, wordish)) && longest >= textualRun && 10*printable >= 9*total && wordish >= textualRun/2 && 2*wordish >= total-punct
}

// maxStrippedPercent is the share of a block that may be invalid UTF-8 bytes, which textual
// strips. Random bytes are about 45% invalid and also hold a control in one byte in eight, which
// is not stripped, so they fail the printable share by far; the cap keeps the strip from
// making a block that is mostly noise look like the text left over.
const maxStrippedPercent = 35

// stripSound reports whether a block that had invalid bytes stripped still reads as text: they
// were at most maxStrippedPercent of it, printable characters at least minPrintablePercent, and
// letters, digits and spaces two thirds of the characters left (a block of punctuation and a few
// letters that the strip left behind is no text).
func stripSound(nBad, n, printBytes, wordish, total int) bool {
	return 100*nBad <= maxStrippedPercent*n && 100*printBytes >= minPrintablePercent*n && wordish >= minStripLetters && 3*wordish >= 2*total
}

// earlyLook is how many bytes textual reads before it gives up on a block that is under
// earlyPrintablePercent printable (a stripped byte is not): random bytes, which are about 44%
// printable, are then read in a few hundred bytes instead of all of them, and text with noise
// (65% printable at the most noise textual accepts) is not lost to it. Noise in front of the
// text does reach it, and resync recovers the text after it, as it does for any garbage in front
// of text.
const (
	earlyLook             = 256
	earlyPrintablePercent = 55
)

// pageBuckets is the number of pages (blocks of 256 code points) of the Basic Multilingual Plane.
const pageBuckets = 256

// pageOf is the page of r; the ideographs, which spread over eighty pages, are one.
func pageOf(r rune) int {
	switch {
	case r >= 0x4e00 && r <= 0x9fff:
		return 0x4e
	case r >= 1<<16:
		return 0
	}
	return int(r >> 8)
}

// oneScript reports whether the letters beyond ASCII of a block with stripped bytes sit on two
// pages for nine in ten: one script's text does (Cyrillic is one page, with ASCII beside it), and
// the other alignments of a stream of such text, which read as letters of every script and an
// invalid byte in five, do not.
func oneScript(pages *[pageBuckets]int32, n int) bool {
	var a, b int32
	for _, c := range pages {
		switch {
		case c > a:
			a, b = c, a
		case c > b:
			b = c
		}
	}
	return 10*int(a+b) >= 9*n
}

// minVariety is how many different letters (of 64 hash buckets) a block needs to be read with
// invalid bytes stripped, and maxLetterShare the most one letter may be of its letters: the zeros
// of a hex string decode to "\xd3M4\xd3M4", which is "M4M4" once stripped, and no text. Text
// puts no letter over a fifth, and a secret with noise between its characters is no less varied.
const (
	minVariety     = 8
	maxLetterShare = 35 // percent
)

func variety(set *[64]int32, letters int) bool {
	distinct, most := 0, int32(0)
	for _, c := range set {
		if c > 0 {
			distinct++
		}
		most = max(most, c)
	}
	return distinct >= minVariety && 100*int(most) <= maxLetterShare*letters
}

// minStripLetters is how many letters, digits and spaces a block needs to be read with invalid
// bytes stripped: the default match threshold is 24 canonical characters, so a block with fewer
// holds no leak, and short windows of random bytes (which hold a stretch of text by chance) do
// not pass.
const minStripLetters = 24

// minPrintablePercent is the share of a block, invalid bytes included, that must be printable
// characters: it keeps a block of mostly punctuation or garbage with a few letters in it (the
// stripped bytes no longer count against the 90% above) from reading as text.
const minPrintablePercent = 60

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
