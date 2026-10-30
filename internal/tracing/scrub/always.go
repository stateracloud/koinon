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
	// authValueRE matches an Authorization value that is not on a line of its own: an optional
	// scheme word and then a token, or a list of name=value parameters (Digest, AWS SigV4,
	// Token token="...") whose values may be quoted.
	authValueRE = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]*[ \t]+)?(?:` +
		`[A-Za-z0-9._~+/-]+=(?:\\*"[^"]*"|[^\s,"']*)(?:,[ \t]*[A-Za-z0-9._~+/-]+=(?:\\*"[^"]*"|[^\s,"']*))*` +
		`|[^\s"'\\` + "`" + `,;)}<>]+)`)
	// pemRE matches a PEM private key block, armour included, raw or with escaped newlines. A
	// block that is cut off before its END line is taken to the end of the text.
	pemRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	// curlUserRE matches the password of curl -u user:password (group 1).
	curlUserRE = regexp.MustCompile(`(?i)curl\b[^\r\n]*?[ \t](?:-u|--user)[ \t]+["']?[^\s:"']+:([^\s"']+)`)
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
		`|ya29\.[A-Za-z0-9_-]{20,}` + // Google OAuth access token
		`|GOCSPX-[A-Za-z0-9_-]{20,}` + // Google OAuth client secret
		`|npm_[A-Za-z0-9]{30,}` + // npm
		`|glsa_[A-Za-z0-9_]{20,}` + // Grafana service account
		`|dop_v1_[a-f0-9]{20,}` + // DigitalOcean
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
	if strings.Contains(in, "PRIVATE KEY-----") {
		for _, m := range pemRE.FindAllStringIndex(in, -1) {
			spans = append(spans, span{m[0], m[1], KindPrivateKey})
		}
	}
	if strings.Contains(in, "://") && strings.IndexByte(in, '@') >= 0 {
		spans = findUserinfo(in, spans)
	}
	if containsFold(in, "curl") {
		for _, m := range curlUserRE.FindAllStringSubmatchIndex(in, -1) {
			spans = append(spans, span{m[2], m[3], KindCredential})
		}
	}
	if hasCredentialWord(in) {
		spans = findHeaders(in, spans)
		spans = findForcedLeaves(in, spans)
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
	for _, p := range [...]string{"sk-", "sk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "hvs.", "hf_", "ya29.", "GOCSPX-", "npm_", "glsa_", "dop_v1_", "AIza", "AKIA"} {
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

// hasCredentialWord gates the header and credential-key rules, in one pass over the text.
func hasCredentialWord(in string) bool {
	for i := 0; i < len(in); i++ {
		switch lower(in[i]) {
		case 'a':
			if hasPrefixFold(in, i, "authorization") || hasPrefixFold(in, i, "api-key") || hasPrefixFold(in, i, "api_key") || hasPrefixFold(in, i, "apikey") {
				return true
			}
		case 'c':
			if hasPrefixFold(in, i, "cookie") {
				return true
			}
		case 'p':
			if hasPrefixFold(in, i, "password") || hasPrefixFold(in, i, "passwd") || hasPrefixFold(in, i, "private_key") ||
				hasPrefixFold(in, i, "private-key") || hasPrefixFold(in, i, "privatekey") {
				return true
			}
		case 's':
			if hasPrefixFold(in, i, "secret") {
				return true
			}
		case 't':
			if hasPrefixFold(in, i, "token") {
				return true
			}
		}
	}
	return false
}

// hasPrefixFold reports whether s[i:] starts with the lower-case ASCII word w, ignoring case.
func hasPrefixFold(s string, i int, w string) bool {
	if i+len(w) > len(s) {
		return false
	}
	for j := 0; j < len(w); j++ {
		if lower(s[i+j]) != w[j] {
			return false
		}
	}
	return true
}

func isNameChar(b byte) bool { return isAlnum(b) || b == '_' || b == '.' || b == '-' }

// normKey normalises a key or header name for comparison: trimmed, camelCase split, lower-cased,
// a dotted path reduced to its last part, "_" read as "-" and a CGI "HTTP_" prefix dropped.
func normKey(key string) string {
	k := strings.TrimSpace(key)
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		if 'A' <= c && c <= 'Z' && i > 0 && 'a' <= k[i-1] && k[i-1] <= 'z' {
			b.WriteByte('-')
		}
		if c == '_' {
			c = '-'
		}
		b.WriteByte(lower(c))
	}
	return strings.TrimPrefix(b.String(), "http-")
}

// headerKind maps a JSON object key (or any header name) to the kind of header it names, or ""
// when it is not one.
func headerKind(key string) Kind {
	switch normKey(key) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "x-auth-token", "x-vault-token",
		"private-token", "x-goog-api-key", "x-gitlab-token":
		return KindAuthHeader
	case "cookie", "set-cookie":
		return KindCookie
	}
	return ""
}

// credentialKind maps a key to KindCredential when its name says it holds a secret: password,
// passwd, secret, token, api key, private key, access/refresh/id/auth/security token, client
// secret, and compounds that end in one (db_password, PGPASSWORD, aws_secret_access_key, accessToken).
func credentialKind(key string) Kind {
	k := normKey(key)
	for _, suf := range [...]string{
		"password", "passwd", "secret", "token", "apikey", "api-key", "private-key",
		"secret-key", "secret-access-key", "privatekey", "secretkey",
	} {
		if strings.HasSuffix(k, suf) {
			return KindCredential
		}
	}
	return ""
}

// nameKind classifies a name found in text.
func nameKind(name string) Kind {
	if k := headerKind(name); k != "" {
		return k
	}
	return credentialKind(name)
}

// maxHeaderValue bounds how far a header or credential value is searched for, so that text with
// no line breaks and many names cannot make the scan quadratic.
const (
	maxHeaderValue     = 8 << 10
	maxCredentialValue = 2 << 10
)

// credWords are the words that end the name of a header or credential key, longest first within a
// first letter. A name is such a word, possibly preceded by more of the identifier (x-vault-token,
// db_password, PGPASSWORD, accessToken).
var credWords = [...]string{
	"authorization", "cookie", "api-key", "api_key", "apikey", "private-key", "private_key", "privatekey",
	"secret-access-key", "secret_access_key", "secret-key", "secret_key", "secretkey", "password", "passwd",
	"secret", "token",
}

// findHeaders appends the value spans of headers and credential-named keys written in text.
func findHeaders(in string, spans []span) []span {
	for i := 0; i < len(in); i++ {
		c := lower(in[i])
		if c != 'a' && c != 'c' && c != 'p' && c != 's' && c != 't' {
			continue
		}
		w := ""
		for _, cand := range credWords {
			if hasPrefixFold(in, i, cand) {
				w = cand
				break
			}
		}
		if w == "" {
			continue
		}
		e := i + len(w)
		sepE, ok := separatorEnd(in, e)
		if !ok {
			i = e - 1
			continue
		}
		nameS, kind := i, nameKind(in[i:e])
		if kind == "" || !leftOK(in, nameS) { // the word ends a longer identifier
			for nameS > 0 && isNameChar(in[nameS-1]) {
				nameS--
			}
			kind = nameKind(in[nameS:e])
		}
		i = e - 1
		if kind == "" || !leftOK(in, nameS) {
			continue
		}
		vs, ve, ok := headerValue(in, nameS, e, e, sepE, kind)
		if !ok || skipHeaderValue(kind, in[vs:ve]) {
			continue
		}
		spans = append(spans, span{vs, ve, kind})
		i = ve - 1 // what follows is inside the value
	}
	return spans
}

// separatorEnd parses what may follow a name: blanks, up to three quote or backslash characters
// (the name's closing quote, escaped or not), blanks, one of : = , then blanks, an optional
// line break with a folded continuation, and up to three opening quote characters. It returns
// the end of that run.
func separatorEnd(in string, e int) (int, bool) {
	j := skipSet(in, e, " \t", 99)
	j = skipSet(in, j, "\\\"'", 3)
	j = skipSet(in, j, " \t", 99)
	if j >= len(in) || (in[j] != ':' && in[j] != '=' && in[j] != ',') {
		return 0, false
	}
	j = skipSet(in, j+1, " \t", 99)
	if k := j; k < len(in) && (in[k] == '\n' || in[k] == '\r') { // value folded onto the next line
		if in[k] == '\r' && k+1 < len(in) && in[k+1] == '\n' {
			k++
		}
		if k+1 < len(in) && (in[k+1] == ' ' || in[k+1] == '\t') {
			j = skipSet(in, k+1, " \t", 99)
		}
	}
	return skipSet(in, j, "\\\"'", 3), true
}

func skipSet(in string, i int, set string, limit int) int {
	for n := 0; n < limit && i < len(in) && strings.IndexByte(set, in[i]) >= 0; n++ {
		i++
	}
	return i
}

// headerValue finds the value that follows a name and separator. Quoted values run to the
// matching closing quote (the escaped form \" included); an HTTP header line runs to the end of
// the line, with obs-fold continuation lines; other values are a token, or for an auth header a
// scheme and a token.
func headerValue(in string, nameS, nameE, sepS, sepE int, kind Kind) (vs, ve int, ok bool) {
	sep := in[sepS:sepE]
	idx := strings.IndexAny(sep, ":=,")
	if idx < 0 {
		return 0, 0, false
	}
	open := strings.TrimLeft(sep[idx+1:], " \t\r\n")
	var q byte
	k := 0
	if open != "" {
		q = open[len(open)-1]
		k = len(open) - 1
	}
	vs = sepE
	window := maxHeaderValue
	if kind == KindCredential {
		window = maxCredentialValue
	}
	hardEnd := min(len(in), vs+window)
	lineEnd := -1 // computed on first use: most values end at a quote long before the line does
	eol := func() int {
		if lineEnd < 0 {
			lineEnd = vs + indexLineEnd(in[vs:hardEnd])
		}
		return lineEnd
	}
	if sep[idx] == ',' && (q == 0 || !nameQuoted(in, nameS)) {
		return 0, 0, false // "password, token" in prose
	}
	if strings.ContainsRune("=:", rune(first(in[vs:]))) && kind == KindCredential {
		return 0, 0, false // := and ==
	}
	if kind == KindCookie && !strings.Contains(in[vs:min(len(in), vs+256)], "=") {
		return 0, 0, false // not cookie-shaped; decided before any long scan
	}
	switch {
	case q != 0:
		if k == 0 {
			ve = vs
			for ve < hardEnd && in[ve] != q && !lineBreakAt(in, ve) {
				if in[ve] == '\\' && ve+1 < hardEnd {
					ve++
				}
				ve++
			}
		} else {
			closing := strings.Repeat("\\", k) + string(q)
			if j := strings.Index(in[vs:eol()], closing); j >= 0 {
				ve = vs + j
			} else {
				ve = eol()
			}
		}
		ve = min(ve, hardEnd)
		if after := strings.TrimLeft(in[min(len(in), ve+1+k):], " \t"); sep[idx] == ',' && strings.HasPrefix(after, ":") {
			return 0, 0, false // that was a key, not a value
		}
	case kind != KindCredential && openQuote(in, nameS) != 0:
		// "Name: value" inside a string (JSON, curl -H "..."): the value runs to the string's end
		oq := openQuote(in, nameS)
		k := quoteEscapes(in, nameS)
		if k == 0 {
			ve = vs
			for ve < hardEnd && in[ve] != oq && !lineBreakAt(in, ve) {
				if in[ve] == '\\' && ve+1 < hardEnd {
					ve++
				}
				ve++
			}
			ve = min(ve, hardEnd)
		} else if j := strings.Index(in[vs:eol()], strings.Repeat("\\", k)+string(oq)); j >= 0 {
			ve = vs + j
		} else {
			ve = eol()
		}
		if kind == KindAuthHeader { // a stray quote before the name does not bound the value
			if m := authValueRE.FindStringIndex(headerPrefix(in, vs, hardEnd)); m != nil {
				ve = max(ve, vs+m[1])
			}
		}
	case kind != KindCredential && headerLine(in, nameS):
		ve = eol()
		for ve < len(in) { // obs-fold: continuation lines start with space or tab
			nl := ve
			if nl < len(in) && in[nl] == '\r' {
				nl++
			}
			if nl+1 < len(in) && in[nl] == '\n' && (in[nl+1] == ' ' || in[nl+1] == '\t') {
				ve = nl + 1 + indexLineEnd(in[nl+1:])
				continue
			}
			break
		}
	case kind == KindCookie:
		ve = eol()
	default:
		ve = vs
		if kind == KindAuthHeader {
			if m := authValueRE.FindStringIndex(headerPrefix(in, vs, hardEnd)); m != nil {
				ve = vs + m[1]
			}
		} else {
			for ve < eol() && !strings.ContainsRune("\"'\\`,;)}<> \t\r\n", rune(in[ve])) {
				ve++
			}
		}
		if kind == KindCredential && sep[idx] == '=' && sepHasSpace(sep, idx) {
			return 0, 0, false // password = getPassword(): code, not a value
		}
		if kind == KindCredential && strings.ContainsAny(in[vs:ve], "()[]") {
			return 0, 0, false
		}
	}
	return vs, ve, ve > vs
}

func first(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

func sepHasSpace(sep string, idx int) bool {
	return (idx > 0 && (sep[idx-1] == ' ' || sep[idx-1] == '\t')) || (idx+1 < len(sep) && (sep[idx+1] == ' ' || sep[idx+1] == '\t'))
}

// indexLineEnd returns the offset of the first line break in s, or len(s). A line break is CR, LF,
// or the two characters backslash-n or backslash-r, which is how one appears in escaped text.
// headerPrefix returns up to 1 KiB of the value that starts at vs, cut at the first line break.
func headerPrefix(in string, vs, hardEnd int) string {
	seg := in[vs:min(hardEnd, vs+1024)]
	return seg[:indexLineEnd(seg)]
}

func lineBreakAt(in string, i int) bool {
	switch in[i] {
	case '\r', '\n':
		return true
	case '\\':
		return i+1 < len(in) && (in[i+1] == 'n' || in[i+1] == 'r')
	}
	return false
}

func indexLineEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r', '\n':
			return i
		case '\\':
			if i+1 < len(s) && (s[i+1] == 'n' || s[i+1] == 'r') {
				return i
			}
		}
	}
	return len(s)
}

// nameQuoted reports whether the name at nameS sits right after a quote (a quoted key).
func nameQuoted(in string, nameS int) bool {
	i := nameS
	for i > 0 && in[i-1] == '\\' {
		i--
	}
	return i > 0 && (in[i-1] == '"' || in[i-1] == '\'')
}

// headerLine reports whether the name starts a line (blanks allowed before it) or follows
// curl's -H "...".
func headerLine(in string, nameS int) bool {
	i := nameS
	for i > 0 {
		switch {
		case in[i-1] == ' ' || in[i-1] == '\t':
			i--
		case i >= 2 && in[i-2] == '\\' && in[i-1] == 't': // an escaped tab
			i -= 2
		default:
			goto done
		}
	}
done:
	if i == 0 || in[i-1] == '\n' || in[i-1] == '\r' {
		return true
	}
	if i >= 2 && in[i-2] == '\\' && (in[i-1] == 'n' || in[i-1] == 'r') { // an escaped line break
		return true
	}
	return false
}

// openQuote returns the quote character that directly precedes the name at nameS (blanks and
// escaped tabs allowed in between), or 0. It is the quote that opens the string the header sits in.
func openQuote(in string, nameS int) byte {
	i := skipBlanksBack(in, nameS)
	if i > 0 && (in[i-1] == '"' || in[i-1] == '\'') {
		return in[i-1]
	}
	return 0
}

// quoteEscapes counts the backslashes that escape the quote found by openQuote: 0 for a plain
// quote, 1 for \", 3 for a quote inside JSON inside JSON.
func quoteEscapes(in string, nameS int) int {
	i := skipBlanksBack(in, nameS) - 1
	k := 0
	for i-k-1 >= 0 && in[i-k-1] == '\\' {
		k++
	}
	return k
}

func skipBlanksBack(in string, i int) int {
	for i > 0 {
		switch {
		case in[i-1] == ' ' || in[i-1] == '\t':
			i--
		case i >= 2 && in[i-2] == '\\' && in[i-1] == 't':
			i -= 2
		default:
			return i
		}
	}
	return i
}

// skipHeaderValue reports values that hold no credential: a bare scheme word ("Authorization:
// Bearer" in prose), a placeholder or variable reference, our own marker, an obvious non-value,
// a package version under a key named cookie, or text that is not cookie-shaped.
func skipHeaderValue(kind Kind, value string) bool {
	v := strings.TrimSpace(value[:min(len(value), 256)]) // the checks are about the start
	if v == "" {
		return true
	}
	fields := strings.Fields(v)
	rest := fields
	switch strings.ToLower(fields[0]) {
	case "bearer", "basic", "digest", "token", "negotiate", "required", "require", "none", "null", "nil", "true", "false":
		rest = fields[1:]
		if len(rest) == 0 {
			return true
		}
	}
	if c := rest[0][0]; c == '$' || c == '{' || c == '<' || c == '[' || c == '%' {
		return true
	}
	switch kind {
	case KindCookie:
		if !strings.Contains(v, "=") {
			return true // "the cookie: chocolate chip"
		}
		for i := 0; i < len(v); i++ {
			if !isDigit(v[i]) && v[i] != '.' {
				return false
			}
		}
		return true
	case KindCredential:
		return len(v) < 4
	}
	return false
}

// credLooking reports whether a value looks like a credential and not like prose: at most a
// scheme word and one token of 8 or more characters, no further words.
func credLooking(v string) bool {
	f := strings.Fields(v)
	if len(f) == 0 || len(f) > 2 {
		return false
	}
	return len(f[len(f)-1]) >= 8
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			return true
		}
	}
	return false
}

// findUserinfo appends the password of every URL with credentials (scheme://user:password@host).
func findUserinfo(in string, spans []span) []span {
	const stop = "/ \t\r\n\"'\\"
	for from := 0; ; {
		i := strings.Index(in[from:], "://")
		if i < 0 {
			return spans
		}
		j := from + i + 3
		from = j
		u := j
		for u < len(in) && in[u] != ':' && in[u] != '@' && strings.IndexByte(stop, in[u]) < 0 {
			u++
		}
		if u == j || u >= len(in) || in[u] != ':' {
			continue
		}
		p := u + 1
		e := p
		for e < len(in) && in[e] != '@' && strings.IndexByte(stop, in[e]) < 0 {
			e++
		}
		if e > p && e < len(in) && in[e] == '@' {
			spans = append(spans, span{p, e, KindCredential})
			from = e
		}
	}
}
