// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"regexp"
	"strings"
)

// The rules that apply at every level. Each regular expression is gated by a cheap literal test
// so that text without the marker never reaches the regexp engine.

const (
	// authNames and cookieNames are the header (or map key) names whose values are credentials.
	// A name may be written with hyphens or underscores and, in CGI and Django environments, with
	// an HTTP_ prefix.
	authNames = `proxy[-_]authorization|authorization|x[-_]api[-_]key|api-key|x[-_]auth[-_]token|` +
		`x[-_]vault[-_]token|private[-_]token|x[-_]goog[-_]api[-_]key|x[-_]gitlab[-_]token`
	cookieNames = `set[-_]cookie|cookie`
)

var (
	// gatewayKeyRE matches a Koinon gateway key. internal/gateway.KeyPrefix is "kgw_" and the
	// material is 32 random bytes in unpadded base64url (43 characters); a shorter tail is still
	// treated as a key, because a truncated or mistyped key is as sensitive. The prefix is
	// repeated here, not imported, to keep this package free of dependencies (a test keeps the two
	// equal). There is no left boundary: a key follows "\n", "%3D" and letters in real output.
	gatewayKeyRE = regexp.MustCompile(`kgw_[A-Za-z0-9_-]{8,}`)
	// placeholderRE matches the OpenShell placeholder a sandbox sees in place of a provider
	// credential (ADR 0006: openshell:resolve:env:KEY). It is not a secret itself, but it names
	// the credential and the sandbox must never be able to turn it into a trace destination's
	// view of the gateway key.
	placeholderRE = regexp.MustCompile(`openshell:resolve:env:[A-Za-z0-9_.-]+`)
	// jwtRE matches a JWT: two base64url JSON segments (they start eyJ, "{\"", or eyA, "{ \"")
	// and a signature that may be empty (alg none). Passports (ADR 0073) are JWTs.
	jwtRE = regexp.MustCompile(`ey[A-Z0-9][A-Za-z0-9_-]{4,}\.ey[A-Z0-9][A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// bearerRE matches "Bearer <token>" with a token long enough to be one ("bearer of bad news"
	// is prose).
	bearerRE = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	// authHeaderRE matches an Authorization-style header (or a key of that name in a map literal,
	// JSON, YAML or env-style text) and captures its value (group 3): an optional scheme word and
	// one token ("Bearer abc", "Basic abc", "abc"), possibly on the next line (YAML). Backslashes
	// are allowed before quotes so that JSON embedded in a JSON string is caught.
	authHeaderRE = regexp.MustCompile(`(?i)(?:http[_-])?(` + authNames + `)([ \t]*[\\"']{0,3}[ \t]*[:=][ \t]*(?:\r?\n[ \t]+)?[\\"']{0,3})` +
		`((?:[A-Za-z][A-Za-z0-9-]*[ \t]+)?[^\s"'\\` + "`" + `,;)}<>]+)`)
	// cookieQuotedRE matches a cookie header written as a quoted key (JSON): the value runs to
	// the closing quote.
	cookieQuotedRE = regexp.MustCompile(`(?i)(?:http[_-])?(` + cookieNames + `)([ \t]*[\\"']{1,3}[ \t]*[:=][ \t]*[\\"']{1,3})([^\r\n"'\\` + "`" + `]+)`)
	// cookiePlainRE matches a cookie header in plain text (HTTP, env, document.cookie=): the value
	// runs to the end of the line, quotes included, because cookie values contain them.
	cookiePlainRE = regexp.MustCompile(`(?i)(?:http[_-])?(` + cookieNames + `)([ \t]*[:=][ \t]*(?:\r?\n[ \t]+)?["']?)([^\r\n]+)`)
	// pairRE matches a header written as a quoted name followed by a quoted value, as code
	// (Header.Set("Authorization", "...")), or as a {"name": ..., "value": ...} record, which can
	// sit in NDJSON or trailing text where the JSON path does not see it. The value is group 4.
	pairRE = regexp.MustCompile(`(?i)([\\]*["'])((?:http[_-])?(?:` + authNames + `|` + cookieNames + `))([\\]*["'])[ \t]*[,:][ \t]*` +
		`(?:[\\]*["']values?[\\]*["'][ \t]*:[ \t]*)?[\\]*["']([^"'\\\r\n]+)`)
	// vendorKeyRE matches the documented shapes of common vendor credentials. They are secrets
	// the org may never have stored in Koinon (a key an agent found on disk), so the known-secret
	// set cannot catch them.
	vendorKeyRE = regexp.MustCompile(`(?:` +
		`sk-(?:ant|or|proj)-[A-Za-z0-9_-]{8,}` + // Anthropic, OpenRouter, OpenAI project keys
		`|sk-[A-Za-z0-9_-]{20,}` + // OpenAI (svcacct, admin, legacy)
		`|[sr]k_live_[A-Za-z0-9]{16,}` + // Stripe
		`|gh[pousr]_[A-Za-z0-9]{20,}` + // GitHub tokens
		`|github_pat_[A-Za-z0-9_]{20,}` + // GitHub fine-grained tokens
		`|glpat-[A-Za-z0-9_-]{16,}` + // GitLab
		`|xox[abprse][.-][A-Za-z0-9.-]{10,}` + // Slack
		`|hvs\.[A-Za-z0-9_-]{16,}` + // Vault service token
		`|hf_[A-Za-z0-9]{20,}` + // Hugging Face
		`|AIza[A-Za-z0-9_-]{20,}` + // Google API key
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
	if strings.Contains(in, ".ey") {
		for _, m := range jwtRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindJWT})
		}
	}
	if containsFold(in, "bearer") {
		for _, m := range bearerRE.FindAllStringIndex(in, -1) {
			tok := in[m[0]:m[1]]
			if hasDigit(tok) || len(tok) >= 24 { // "Bearer resource_metadata=..." is not a token
				spans = append(spans, span{m[0], m[1], KindBearer})
			}
		}
	}
	hasAuth := containsFold(in, "authorization") || containsFold(in, "api-key") || containsFold(in, "api_key") ||
		containsFold(in, "-token") || containsFold(in, "_token")
	hasCookie := containsFold(in, "cookie")
	if hasAuth {
		spans = headerSpans(in, authHeaderRE, 1, 3, spans)
	}
	if hasCookie {
		spans = headerSpans(in, cookieQuotedRE, 1, 3, spans)
		spans = headerSpans(in, cookiePlainRE, 1, 3, spans)
	}
	if hasAuth || hasCookie {
		spans = headerSpans(in, pairRE, 2, 4, spans)
	}
	if hasVendorPrefix(in) {
		for _, m := range vendorKeyRE.FindAllStringIndex(in, -1) {
			if leftOK(in, m[0]) {
				spans = append(spans, span{m[0], m[1], KindAPIKey})
			}
		}
	}
	return spans
}

// leftOK reports whether a token may start at i: after a non-word character, or after the tail of
// an escape that stands for one ("\n", "\t", "\r", "\"", "\/", and "%XX").
func leftOK(in string, i int) bool {
	if i == 0 {
		return true
	}
	c := in[i-1]
	if !isAlnum(c) && c != '_' {
		return true
	}
	if i >= 2 && in[i-2] == '\\' && (c == 'n' || c == 't' || c == 'r' || c == '"' || c == '/') {
		return true
	}
	return i >= 3 && in[i-3] == '%' && isHex(in[i-2]) && isHex(c)
}

func isHex(b byte) bool {
	return isDigit(b) || ('a' <= b && b <= 'f') || ('A' <= b && b <= 'F')
}

func hasVendorPrefix(in string) bool {
	for _, p := range [...]string{"sk-", "sk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "hvs.", "hf_", "AIza", "AKIA"} {
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

// headerKind maps a JSON object key (or any header name) to the kind of header it names, or ""
// when it is not one. The name is trimmed and lower-cased, a dotted path ("x.authorization",
// "request.headers.cookie") is reduced to its last part, "_" is read as "-" and a CGI "HTTP_"
// prefix is dropped.
func headerKind(key string) Kind {
	k := strings.ToLower(strings.TrimSpace(key))
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	k = strings.TrimPrefix(strings.ReplaceAll(k, "_", "-"), "http-")
	switch k {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "x-auth-token", "x-vault-token",
		"private-token", "x-goog-api-key", "x-gitlab-token":
		return KindAuthHeader
	case "cookie", "set-cookie":
		return KindCookie
	}
	return ""
}

// headerSpans appends the value span of every match of re. nameGroup and valueGroup are the
// submatch numbers of the header name and its value.
func headerSpans(in string, re *regexp.Regexp, nameGroup, valueGroup int, spans []span) []span {
	for _, m := range re.FindAllStringSubmatchIndex(in, -1) {
		if nameGroup == 1 && !leftOK(in, m[0]) { // "mycookie: x" is not a header
			continue
		}
		kind := headerKind(in[m[2*nameGroup]:m[2*nameGroup+1]])
		if kind == "" {
			continue
		}
		vs, ve := m[2*valueGroup], m[2*valueGroup+1]
		if skipHeaderValue(kind, in[vs:ve]) {
			continue
		}
		spans = append(spans, span{vs, ve, kind})
	}
	return spans
}

// skipHeaderValue reports values that hold no credential: a bare scheme word ("Authorization:
// Bearer" in prose), a variable reference, or a package version under a key named cookie.
func skipHeaderValue(kind Kind, value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bearer", "basic", "digest", "token", "negotiate", "required", "require", "none":
		return true
	}
	if i := strings.LastIndexAny(value, " \t"); i >= 0 && i+1 < len(value) && (value[i+1] == '$' || value[i+1] == '{') {
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
