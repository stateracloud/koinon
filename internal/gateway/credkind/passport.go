// SPDX-License-Identifier: Apache-2.0

package credkind

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

// PassportType is the JOSE typ of an agent passport (ADR 0073 decision 3).
const PassportType = "koinon-passport+jwt"

// jwtRE matches a compact JWS at the start of a text: a base64url JSON header (it starts "ey" or "ew", the
// encoding of `{"`, `{ ` or `{` and a line break or tab), a payload and a signature that can be empty or cut. The
// header is decoded to check its typ, so the shape alone redacts nothing.
var jwtRE = regexp.MustCompile(`^e[wy][A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// schemeRE matches one auth scheme word and the space after it ("Bearer ", "Passport ").
var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*\s+`)

// maxHeader is the longest base64url header that FindPassports decodes. A passport header is about 100 bytes.
// The bound keeps the search linear in the length of the text.
const maxHeader = 512

// IsPassport reports whether v, after an optional auth scheme word ("Bearer ", "Passport "), is one passport and
// nothing else.
func IsPassport(v string) bool {
	v = strings.TrimSpace(v)
	v = v[len(schemeRE.FindString(v)):]
	loc := jwtRE.FindStringIndex(v)
	return loc != nil && loc[1] == len(v) && passportHeader(v[:strings.IndexByte(v, '.')])
}

// FindPassports returns the start and end of each passport in s. A JWT of another typ is not returned. A passport
// that directly follows other base64url text (for example "key_" or "honey") is found too: each start in the
// last maxHeader bytes before a dot is tried.
func FindPassports(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		k := strings.IndexByte(s[i:], 'e')
		if k < 0 {
			break
		}
		run := i + k
		end := run
		for end < len(s) && isB64URL(s[end]) {
			end++
		}
		next := end + 1
		if end < len(s) && s[end] == '.' {
			for start := max(run, end-maxHeader); start+10 <= end; start++ {
				if s[start] != 'e' || (s[start+1] != 'y' && s[start+1] != 'w') || !passportHeader(s[start:end]) {
					continue
				}
				if m := jwtRE.FindStringIndex(s[start:]); m != nil {
					out = append(out, [2]int{start, start + m[1]})
					next = start + m[1]
				}
				break
			}
		}
		i = next
	}
	return out
}

func isB64URL(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// passportHeader reports whether seg, a base64url JOSE header, has the passport typ. The typ is compared without
// case, and the "application/" prefix may be present (RFC 7515 section 4.1.9).
func passportHeader(seg string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return false
	}
	var h struct {
		Typ string `json:"typ"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return false
	}
	typ := strings.TrimSpace(h.Typ)
	if len(typ) > len("application/") && strings.EqualFold(typ[:len("application/")], "application/") {
		typ = typ[len("application/"):]
	}
	return strings.EqualFold(typ, PassportType)
}
