// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
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

// The shortest base64 run decoded is 16 characters (12 bytes); the shortest hex run 16 (8 bytes).
const (
	minBase64Run = 16
	minHexRun    = 16
	// textualRun is the stretch of printable characters a decoding needs to replace its run.
	textualRun = 12
)

type decoder struct {
	name string
	fn   func(string) []string
}

var decoders = []decoder{
	{"percent", decodePercent},
	{"base64", decodeBase64},
	{"hex", decodeHex},
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

// decodeBase64 returns, for each 4-character alignment, the text with every base64 run that
// decodes to text at that alignment replaced by its decoding. Both alphabets are accepted in
// one run ("-" and "_" read as "+" and "/"), padding is optional.
func decodeBase64(s string) []string {
	runs := findRuns(s, isBase64Char, minBase64Run)
	if len(runs) == 0 {
		return nil
	}
	var out []string
	for off := range 4 {
		var b strings.Builder
		last := 0
		changed := false
		for _, r := range runs {
			start := r[0] + off
			if r[1]-start < minBase64Run {
				continue
			}
			dec, ok := base64Text(s[start:r[1]])
			if !ok {
				continue
			}
			if !changed {
				b.Grow(len(s))
				changed = true
			}
			b.WriteString(s[last:start])
			b.WriteString(dec)
			last = r[1]
		}
		if changed {
			b.WriteString(s[last:])
			out = append(out, b.String())
		}
	}
	return out
}

func base64Text(run string) (string, bool) {
	if len(run)%4 == 1 {
		run = run[:len(run)-1]
	}
	buf := []byte(run)
	for i, c := range buf {
		switch c {
		case '-':
			buf[i] = '+'
		case '_':
			buf[i] = '/'
		}
	}
	dec := make([]byte, base64.RawStdEncoding.DecodedLen(len(buf)))
	n, err := base64.RawStdEncoding.Decode(dec, buf)
	if err != nil {
		return "", false
	}
	dec = dec[:n]
	if !textual(dec) {
		return "", false
	}
	return string(dec), true
}

// decodeHex returns, for each 2-character alignment, the text with every hex run that decodes to
// text replaced by its decoding.
func decodeHex(s string) []string {
	runs := findRuns(s, isHex, minHexRun)
	if len(runs) == 0 {
		return nil
	}
	var out []string
	for off := range 2 {
		var b strings.Builder
		last := 0
		changed := false
		for _, r := range runs {
			start := r[0] + off
			end := r[1]
			if (end-start)%2 == 1 {
				end--
			}
			if end-start < minHexRun {
				continue
			}
			dec := make([]byte, (end-start)/2)
			for i := range dec {
				dec[i] = unhex(s[start+2*i])<<4 | unhex(s[start+2*i+1])
			}
			if !textual(dec) {
				continue
			}
			if !changed {
				b.Grow(len(s))
				changed = true
			}
			b.WriteString(s[last:start])
			b.Write(dec)
			last = end
		}
		if changed {
			b.WriteString(s[last:])
			out = append(out, b.String())
		}
	}
	return out
}

// findRuns returns the [start, end) byte ranges of the maximal runs of in-class bytes at least
// min long.
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

// textual reports whether b holds text: textualRun printable characters (or ordinary
// whitespace) in a row. A misaligned or garbage prefix does not hide text after it.
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

func isBase64Char(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '+' || c == '/' || c == '-' || c == '_'
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
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
