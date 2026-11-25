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

// jwtRE matches a compact JWS: a base64url JSON header (it starts "ey", the encoding of `{"` or `{ `), a payload
// and a signature that can be empty or cut. The header is decoded to check its typ, so the shape alone redacts
// nothing.
var jwtRE = regexp.MustCompile(`ey[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// IsPassport reports whether v, after an optional "Bearer " word, is one passport and nothing else.
func IsPassport(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) > len("bearer ") && strings.EqualFold(v[:len("bearer ")], "bearer ") {
		v = strings.TrimSpace(v[len("bearer "):])
	}
	loc := jwtRE.FindStringIndex(v)
	return loc != nil && loc[0] == 0 && loc[1] == len(v) && passportHeader(v[:strings.IndexByte(v, '.')])
}

// FindPassports returns the start and end of each passport in s. A JWT of another typ is not returned.
func FindPassports(s string) [][2]int {
	if !strings.Contains(s, "ey") {
		return nil
	}
	var out [][2]int
	for _, m := range jwtRE.FindAllStringIndex(s, -1) {
		if passportHeader(s[m[0] : m[0]+strings.IndexByte(s[m[0]:m[1]], '.')]) {
			out = append(out, [2]int{m[0], m[1]})
		}
	}
	return out
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
