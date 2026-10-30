// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"regexp"
	"strings"
)

// The rules that apply at every level. Each regular expression is gated by a cheap literal test
// so that text without the marker never reaches the regexp engine.

var (
	// gatewayKeyRE matches a Koinon gateway key. internal/gateway.KeyPrefix is "kgw_" and the
	// material is 32 random bytes in unpadded base64url (43 characters); a shorter tail is still
	// treated as a key, because a truncated or mistyped key is as sensitive. The prefix is
	// repeated here, not imported, to keep this package free of dependencies (a test keeps the two
	// equal).
	gatewayKeyRE = regexp.MustCompile(`kgw_[A-Za-z0-9_-]{8,}`)
	// placeholderRE matches the OpenShell placeholder a sandbox sees in place of a provider
	// credential (ADR 0006: openshell:resolve:env:KEY). It is not a secret itself, but it names
	// the credential and the sandbox must never be able to turn it into a trace destination's
	// view of the gateway key.
	placeholderRE = regexp.MustCompile(`openshell:resolve:env:[A-Za-z0-9_]+`)
	// jwtRE matches a JWT: two base64url JSON segments (they start eyJ, "{\"") and a signature
	// that may be empty (alg none). Passports (ADR 0073) are JWTs.
	jwtRE = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// bearerRE matches "Bearer <token>" with a token long enough to be one ("bearer of bad news"
	// is prose).
	bearerRE = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	// authHeaderRE matches an Authorization-style header (or a key of that name in a map literal,
	// JSON, YAML or env-style text) and captures its value (group 3): an optional scheme word and
	// one token ("Bearer abc", "Basic abc", "abc"). Backslashes are allowed before quotes so that
	// JSON embedded in a JSON string is caught.
	authHeaderRE = regexp.MustCompile(`(?i)\b(proxy-authorization|authorization|x-api-key|x-auth-token)([\\"']{0,3}[ \t]*[:=][ \t]*[\\"']{0,3})((?:[A-Za-z][A-Za-z0-9-]* +)?[^\s"'\\` + "`" + `,;)}<>]+)`)
	// cookieRE matches a Cookie or Set-Cookie header and captures its value, which runs to the
	// end of the line or the closing quote (group 3).
	cookieRE = regexp.MustCompile(`(?i)\b(set-cookie|cookie)([\\"']{0,3}[ \t]*[:=][ \t]*[\\"']{0,3})([^\r\n"'\\` + "`" + `]+)`)
	// vendorKeyRE matches the documented shapes of common vendor credentials. They are secrets
	// the org may never have stored in Koinon (a key an agent found on disk), so the known-secret
	// set cannot catch them.
	vendorKeyRE = regexp.MustCompile(`\b(?:` +
		`sk-(?:ant|or|proj)-[A-Za-z0-9_-]{8,}` + // Anthropic, OpenRouter, OpenAI project keys
		`|sk-[A-Za-z0-9]{20,}` + // OpenAI
		`|gh[pousr]_[A-Za-z0-9]{20,}` + // GitHub tokens
		`|github_pat_[A-Za-z0-9_]{20,}` + // GitHub fine-grained tokens
		`|glpat-[A-Za-z0-9_-]{16,}` + // GitLab
		`|xox[abprs]-[A-Za-z0-9-]{10,}` + // Slack
		`|AKIA[0-9A-Z]{16}` + // AWS access key ID
		`)`)
)

// findAlways appends the spans of the always rules.
func findAlways(in string, spans []span) []span {
	if strings.Contains(in, "kgw_") {
		for _, m := range gatewayKeyRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindGatewayKey})
		}
	}
	if strings.Contains(in, "openshell:resolve:") {
		for _, m := range placeholderRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindPlaceholder})
		}
	}
	if strings.Contains(in, "eyJ") {
		for _, m := range jwtRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindJWT})
		}
	}
	if containsFold(in, "bearer") {
		for _, m := range bearerRE.FindAllStringIndex(in, -1) {
			if hasDigit(in[m[0]:m[1]]) { // "Bearer resource_metadata=..." is not a token
				spans = append(spans, span{m[0], m[1], KindBearer})
			}
		}
	}
	if containsFold(in, "authorization") || containsFold(in, "x-api-key") || containsFold(in, "x-auth-token") {
		spans = headerSpans(in, authHeaderRE, KindAuthHeader, spans)
	}
	if containsFold(in, "cookie") {
		spans = headerSpans(in, cookieRE, KindCookie, spans)
	}
	if hasVendorPrefix(in) {
		for _, m := range vendorKeyRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindAPIKey})
		}
	}
	return spans
}

func hasVendorPrefix(in string) bool {
	for _, p := range [...]string{"sk-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "AKIA"} {
		if strings.Contains(in, p) {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains the lower-case ASCII word sub, ignoring case, without
// allocating.
func containsFold(s, sub string) bool {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if lower(s[i]) != sub[0] {
			continue
		}
		j := 1
		for j < n && lower(s[i+j]) == sub[j] {
			j++
		}
		if j == n {
			return true
		}
	}
	return false
}

func lower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + 32
	}
	return b
}

// headerKind maps a JSON object key to the kind of header it names, or "" when it is not one.
func headerKind(key string) Kind {
	switch strings.ToLower(key) {
	case "authorization", "proxy-authorization", "x-api-key", "x-auth-token":
		return KindAuthHeader
	case "cookie", "set-cookie":
		return KindCookie
	}
	return ""
}

// headerSpans appends the value span of every match of re. A header name preceded by a dot
// ("traffic.authorization") is a field path in prose or code, not a header.
func headerSpans(in string, re *regexp.Regexp, kind Kind, spans []span) []span {
	for _, m := range re.FindAllStringSubmatchIndex(in, -1) {
		if m[0] > 0 && in[m[0]-1] == '.' {
			continue
		}
		value := in[m[6]:m[7]]
		if skipHeaderValue(kind, value) {
			continue
		}
		spans = append(spans, span{m[6], m[7], kind})
	}
	return spans
}

// skipHeaderValue reports values that hold no credential: a bare scheme word ("Authorization:
// Bearer" in prose), a variable reference, or a package version under a key named cookie.
func skipHeaderValue(kind Kind, value string) bool {
	switch strings.ToLower(value) {
	case "bearer", "basic", "digest", "token", "negotiate", "required", "require", "none":
		return true
	}
	if i := strings.LastIndexByte(value, ' '); i >= 0 && value[i+1:] != "" && (value[i+1] == '$' || value[i+1] == '{') {
		return true
	}
	if value[0] == '$' || value[0] == '{' {
		return true
	}
	if kind == KindCookie {
		for i := 0; i < len(value); i++ {
			if !isDigit(value[i]) && value[i] != '.' {
				return false
			}
		}
		return true
	}
	return false
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			return true
		}
	}
	return false
}
