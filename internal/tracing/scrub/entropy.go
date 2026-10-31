// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"math"
	"strings"
)

// The high-entropy token rule (LevelRedacted). It looks for an unbroken run of base64, base64url
// and alphanumeric characters that is long, mixes letters and digits, and is spread over many
// symbols. These are the numbers, chosen against the false-positive corpus in the tests:
const (
	// MinTokenLen is the shortest run considered. Random 24-character base64 carries about 144
	// bits; common identifiers and words rarely run this long without a break.
	MinTokenLen = 24
	// MinLowerTokenLen is the shortest run considered when it has no upper-case letter
	// (lower-case and digits only, "hex-like"): such runs are common in slugs and IDs, so more
	// length is needed.
	MinLowerTokenLen = 32
	// TokenEntropyBits is the Shannon entropy per character (in bits) a run must reach. Random
	// base64 of 24 to 64 characters averages 4.2 to 5.5; English words, camelCase identifiers and
	// repeated patterns stay under 3.8.
	TokenEntropyBits = 3.7
	// maxRunLen is the longest run scored; a longer one is a data blob and is left alone.
	maxRunLen = 4096
)

func isTokenChar(b byte) bool {
	return isAlnum(b) || b == '+' || b == '/' || b == '_' || b == '-'
}

// findTokens appends the spans of high-entropy tokens.
func findTokens(in string, spans []span) []span {
	for i := 0; i < len(in); {
		if !isTokenChar(in[i]) {
			i++
			continue
		}
		j := i
		for j < len(in) && isTokenChar(in[j]) {
			j++
		}
		if j-i >= MinTokenLen && !inDataURL(in, i) && !digestPrefix(in, i) {
			spans = scoreRun(in, i, j, spans)
		}
		i = j
	}
	return spans
}

// inDataURL reports whether a run at i is the payload of a data:...;base64, URL.
func inDataURL(in string, i int) bool {
	if i < len("base64,") || in[i-len("base64,"):i] != "base64," {
		return false
	}
	head := in[max(0, i-160):i]
	d := strings.LastIndex(head, "data:")
	return d >= 0 && strings.IndexByte(head[d:], ';') >= 0 && !strings.ContainsAny(head[d:], " \t\r\n")
}

// digestPrefix reports whether the run at i follows a digest label: go.sum's h1: and the OCI
// sha256:, sha384:, sha512:. What follows is a public hash.
func digestPrefix(in string, i int) bool {
	for _, p := range [...]string{"h1:", "sha256:", "sha384:", "sha512:"} {
		if i >= len(p) && in[i-len(p):i] == p {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// scoreRun judges the run in[i:j]. A run with three or more slashes is a path or a URL tail, and
// each segment is judged alone; otherwise the run is judged whole.
func scoreRun(in string, i, j int, spans []span) []span {
	run := in[i:j]
	if hasSRIPrefix(run) {
		return spans // a public digest, however many slashes it has
	}
	if j-i > maxRunLen {
		// A very long run is judged in pieces: by slash-separated segment, and a segment that is
		// itself longer than maxRunLen in windows. Judging it whole would let a credential
		// hide behind padding.
		off := i
		for _, seg := range strings.Split(run, "/") {
			for w := 0; w < len(seg); w += maxRunLen / 2 {
				end := min(len(seg), w+maxRunLen)
				if end-w >= MinTokenLen && tokenLike(seg[w:end]) {
					spans = append(spans, span{off + w, off + end, KindToken})
				}
				if end == len(seg) {
					break
				}
			}
			off += len(seg) + 1
		}
		return spans
	}
	if strings.Count(run, "/") >= 3 {
		off := i
		for _, seg := range strings.Split(run, "/") {
			if len(seg) >= MinTokenLen && tokenLike(seg) {
				spans = append(spans, span{off, off + len(seg), KindToken})
			}
			off += len(seg) + 1
		}
		return spans
	}
	// leading all-lower-case segments are a path or a host, not part of a token
	for {
		slash := strings.IndexByte(run, '/')
		if slash <= 0 || !pathWord(run[:slash]) {
			break
		}
		run = run[slash+1:]
		i += slash + 1
	}
	if tokenLike(run) {
		spans = append(spans, span{i, j, KindToken})
	}
	return spans
}

// tokenLike applies the rule to one run.
func tokenLike(run string) bool {
	run = strings.TrimRight(run, "=")
	n := len(run)
	if n < MinTokenLen {
		return false
	}
	var upper, lower, digit bool
	hex := true
	for i := 0; i < n; i++ {
		c := run[i]
		switch {
		case 'A' <= c && c <= 'Z':
			upper = true
			hex = hex && c <= 'F'
		case 'a' <= c && c <= 'z':
			lower = true
			hex = hex && c <= 'f'
		case isDigit(c):
			digit = true
		default:
			hex = false
		}
	}
	if !digit || (!upper && !lower) {
		return false // identifiers and words have no digits; numbers have no letters
	}
	if hex && (n == 40 || n == 64 || (!upper && n <= 40)) {
		// git SHA-1 and SHA-256 names, plain digests, and the lower-case 32- and 16-digit hex of
		// abbreviated git SHAs, trace and span IDs: public, and exactly what a trace reader needs to see. A hex secret of
		// these lengths is therefore not caught by this rule (see the package comment).
		return false
	}
	if isUUID(run) || publicID(run) {
		return false
	}
	if looksLikeImage(run) {
		return false
	}
	if !upper && (n < MinLowerTokenLen || strings.ContainsAny(run, "-_/")) {
		return false // lower-case slugs and resource names; only a plain [a-z0-9] run counts
	}
	if digitIslands(run) < minIslands(n) || camelCaseWords(run) || lowerWords(run) {
		return false // a name with a version suffix, or words run together
	}
	return entropy(run) >= TokenEntropyBits
}

// hasSRIPrefix reports whether run is a Subresource Integrity or npm lockfile hash
// (sha512-<base64>), a public digest.
func hasSRIPrefix(run string) bool {
	return strings.HasPrefix(run, "sha256-") || strings.HasPrefix(run, "sha384-") || strings.HasPrefix(run, "sha512-")
}

// pathWord reports whether s is a lower-case word of letters, dots, hyphens and underscores.
func pathWord(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isDigit(c) && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// looksLikeImage recognises base64 that starts with the magic bytes of PNG, JPEG, GIF, WebP or
// PDF. A raw image in a trace is content, not a credential.
func looksLikeImage(s string) bool {
	for _, p := range [...]string{"iVBORw0KGgo", "/9j/", "R0lGOD", "UklGR", "JVBERi0"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// digitIslands counts the separate runs of digits in s. Random tokens scatter their digits
// (about 15% of base64 symbols are digits); names like ListPrices200JSONResponse have one run.
func digitIslands(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) && (i == 0 || !isDigit(s[i-1])) {
			n++
		}
	}
	return n
}

// minIslands is how many digit runs a token of n characters needs: a long run can get by with
// one, since a long random token without a second digit run is rare (under 1 in 100 at 43).
func minIslands(n int) int {
	if n >= 40 {
		return 1
	}
	return 2
}

// camelCaseWords reports whether the run reads as capitalised words: at least three upper-case
// letters and nearly every one followed by a lower-case letter. In random base64 about two in
// five are.
func camelCaseWords(s string) bool {
	upper, followed := 0, 0
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			upper++
			if i+1 < len(s) && 'a' <= s[i+1] && s[i+1] <= 'z' {
				followed++
			}
		}
	}
	return upper >= 3 && followed*10 >= upper*8
}

// publicID recognises shapes that are identifiers, not credentials: a 26-character Crockford
// base32 ULID, and a base64 DER ECDSA signature (cosign, sigstore), which starts MEUCI, MEQCI or MEYCI.
func publicID(s string) bool {
	if len(s) == 26 {
		ok := true
		for i := 0; i < 26 && ok; i++ {
			c := s[i]
			ok = isDigit(c) || (c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U')
		}
		if ok && s[0] <= '7' {
			return true
		}
	}
	return strings.HasPrefix(s, "MEUCI") || strings.HasPrefix(s, "MEQCI") || strings.HasPrefix(s, "MEYCI")
}

// lowerWords reports whether at least 40% of the run is lower-case letters in stretches of four
// or more, the way the words of an identifier (TestScrubJSON2Roundtrip4Cases) are; random
// base64 has few such stretches.
func lowerWords(s string) bool {
	run, in := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && 'a' <= s[i] && s[i] <= 'z' {
			run++
			continue
		}
		if run >= 4 {
			in += run
		}
		run = 0
	}
	return in*10 >= len(s)*4
}
