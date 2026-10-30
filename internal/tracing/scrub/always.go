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
	// paramContRE matches further name=value parameters of an Authorization list.
	paramContRE = regexp.MustCompile(`^(?:,[ \t]*[A-Za-z0-9._~+/-]+=(?:\\*"[^"]*"|[^\s,"']*))+`)
	// pemRE matches a PEM private key block, armour included, raw or with escaped newlines. A
	// block that is cut off before its END line is taken to the end of the text.
	pemRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
	// curlUserRE matches the password of curl -u user:password (group 1).
	curlUserRE = regexp.MustCompile(`(?i)curl\b[^\r\n]*?[ \t](?:-u[ \t]*|--user(?:[ \t]+|=))["']?[^\s:"']+:([^\s"']+)`)
	// passwordFlagRE matches a password given as the next argument (mysql --password X); the
	// attached --password=X form is a key=value pair and is found with the other credential names.
	passwordFlagRE = regexp.MustCompile(`(?i)--pass(?:word|wd)[ \t]+["']?([^\s"'-][^\s"']*)`)
	// mysqlPassRE matches mysql -pSECRET (the password is attached to the flag).
	mysqlPassRE = regexp.MustCompile(`(?i)\bmysql[a-z]*\b[^\r\n]*?[ \t]-p([^\s-][^\s]*)`)
	// netrcRE and netrcLineRE match a .netrc password, on the machine line or on a line of its own.
	netrcRE     = regexp.MustCompile(`(?i)\b(?:machine|login)\b[^\r\n]*?\bpassword[ \t]+([^\s]+)`)
	netrcLineRE = regexp.MustCompile(`(?im)^[ \t]*password[ \t]+([^\s]+)[ \t]*$`)
	// yamlPairRE matches a Kubernetes env entry written as "name: DB_PASSWORD" with "value:" on
	// the next line (group 1 the name, group 3 the value).
	yamlPairRE = regexp.MustCompile(`(?i)\bname:[ \t]*["']?([A-Za-z0-9_.-]+)["']?[ \t]*\r?\n[ \t]*(?:- )?value:[ \t]*(["']?)([^\r\n"']+)`)
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
	if strings.Contains(in, "PRIVATE KEY") {
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
	if containsFold(in, "--pass") {
		for _, m := range passwordFlagRE.FindAllStringSubmatchIndex(in, -1) {
			spans = append(spans, span{m[2], m[3], KindCredential})
		}
	}
	if containsFold(in, "mysql") {
		for _, m := range mysqlPassRE.FindAllStringSubmatchIndex(in, -1) {
			spans = append(spans, span{m[2], m[3], KindCredential})
		}
	}
	if containsFold(in, "password") && (containsFold(in, "machine") || containsFold(in, "login")) {
		for _, re := range []*regexp.Regexp{netrcRE, netrcLineRE} {
			for _, m := range re.FindAllStringSubmatchIndex(in, -1) {
				spans = append(spans, span{m[2], m[3], KindCredential})
			}
		}
	}
	if containsFold(in, "name:") && containsFold(in, "value:") {
		for _, m := range yamlPairRE.FindAllStringSubmatchIndex(in, -1) {
			if credentialKind(in[m[2]:m[3]]) != "" || headerKind(in[m[2]:m[3]]) != "" {
				spans = append(spans, span{m[6], m[7], KindCredential})
			}
		}
	}
	spans = findEncodedBlobs(in, spans)
	var sawWord bool
	if spans, sawWord = findHeaders(in, spans); sawWord {
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
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case 's', 'r':
			if strings.HasPrefix(in[i:], "sk-") || strings.HasPrefix(in[i:], "sk_") || strings.HasPrefix(in[i:], "rk_") {
				return true
			}
		case 'g':
			if strings.HasPrefix(in[i:], "ghp_") || strings.HasPrefix(in[i:], "gho_") || strings.HasPrefix(in[i:], "ghu_") ||
				strings.HasPrefix(in[i:], "ghs_") || strings.HasPrefix(in[i:], "ghr_") || strings.HasPrefix(in[i:], "github_pat_") ||
				strings.HasPrefix(in[i:], "glpat-") || strings.HasPrefix(in[i:], "glsa_") {
				return true
			}
		case 'x':
			if strings.HasPrefix(in[i:], "xox") {
				return true
			}
		case 'h':
			if strings.HasPrefix(in[i:], "hvs.") || strings.HasPrefix(in[i:], "hf_") {
				return true
			}
		case 'y':
			if strings.HasPrefix(in[i:], "ya29.") {
				return true
			}
		case 'n':
			if strings.HasPrefix(in[i:], "npm_") {
				return true
			}
		case 'd':
			if strings.HasPrefix(in[i:], "dop_v1_") {
				return true
			}
		case 'G':
			if strings.HasPrefix(in[i:], "GOCSPX-") {
				return true
			}
		case 'A':
			if strings.HasPrefix(in[i:], "AIza") || strings.HasPrefix(in[i:], "AKIA") {
				return true
			}
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
	switch k {
	case "auth", "identitytoken", "registrytoken": // a Docker config's credentials
		return KindCredential
	}
	for _, ref := range [...]string{"existing-secret", "pull-secret", "tls-secret", "existingsecret"} {
		if strings.HasSuffix(k, ref) {
			return "" // names a Kubernetes Secret object, not a value
		}
	}
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

// credWords are the words that end the name of a header or credential key, longest first within a
// first letter. A name is such a word, possibly preceded by more of the identifier (x-vault-token,
// db_password, PGPASSWORD, accessToken).
var credWords = [...]string{
	"authorization", "cookie", "api-key", "api_key", "apikey", "private-key", "private_key", "privatekey",
	"secret-access-key", "secret_access_key", "secret-key", "secret_key", "secretkey", "password", "passwd",
	"secret", "token", "auth",
}

// findHeaders appends the value spans of headers and credential-named keys written in text.
func findHeaders(in string, spans []span) (_ []span, sawWord bool) {
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
		sawWord = true
		e := i + len(w)
		sepE, ok := separatorEnd(in, e)
		if !ok {
			i = e - 1
			continue
		}
		// the whole identifier the word ends (x-vault-token, db_password) names the key; failing
		// that, the word alone (after an escape such as \n there is no identifier boundary)
		nameS := i
		for nameS > 0 && isNameChar(in[nameS-1]) {
			nameS--
		}
		kind := nameKind(in[nameS:e])
		if kind == "" || !leftOK(in, nameS) {
			nameS, kind = i, nameKind(in[i:e])
			if !leftOK(in, nameS) {
				kind = ""
			}
		}
		i = e - 1
		if kind == "" {
			continue
		}
		if w == "auth" && (nameS != i-len(w)+1 || !nameQuoted(in, nameS)) { // only a quoted "auth" key (Docker config)
			continue
		}
		vs, ve, ok := headerValue(in, nameS, e, e, sepE, kind)
		if !ok || skipHeaderValue(kind, in[vs:ve]) {
			continue
		}
		spans = append(spans, span{vs, ve, kind})
		i = ve - 1 // what follows is inside the value
	}
	return spans, sawWord
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
			if next := skipSet(in, k+1, " \t", 99); !startsKeyLine(in, next) {
				j = next
			}
		}
	}
	return skipSet(in, j, "\\\"'", 3), true
}

// startsKeyLine reports whether the text at i begins a "key:" line (YAML), which is the next
// entry and not the folded continuation of a value.
func startsKeyLine(in string, i int) bool {
	j := i
	for j < len(in) && isNameChar(in[j]) {
		j++
	}
	return j > i && j < len(in) && in[j] == ':' && (j+1 >= len(in) || in[j+1] == ' ' || in[j+1] == '\n' || in[j+1] == '\r')
}

func skipSet(in string, i int, set string, limit int) int {
	for n := 0; n < limit && i < len(in) && strings.IndexByte(set, in[i]) >= 0; n++ {
		i++
	}
	return i
}

// headerValue finds the value that follows a name and separator. Quoted values run to the
// matching closing quote (the escaped form \" included); an HTTP header line runs to the end of
// the line, with obs-fold continuation lines; an auth header elsewhere is a scheme and a token or
// a parameter list; a credential is the run of non-blank characters. There is no length window:
// a value is redacted to its real end. Matches that cannot be values are rejected from a short
// prefix before any long scan, and the caller resumes after an accepted value, so the total
// work stays linear.
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
	if sep[idx] == ',' && (q == 0 || !nameQuoted(in, nameS)) {
		return 0, 0, false // "password, token" in prose
	}
	if kind == KindCredential && strings.IndexByte("=:", first(in[vs:])) >= 0 && vs < len(in) {
		return 0, 0, false // := and ==
	}
	prefix := headerPrefix(in, vs)
	if q == 0 && skipHeaderValue(kind, prefix) && !foldFollows(in, vs+len(prefix)) {
		return 0, 0, false // decided from the start of the value, before any long scan
	}
	if kind == KindCookie && !strings.Contains(in[vs:min(len(in), vs+256)], "=") {
		return 0, 0, false // not cookie-shaped
	}
	switch {
	case q != 0:
		ve = scanQuoted(in, vs, q, k)
		if after := strings.TrimLeft(in[min(len(in), ve+1+k):], " \t"); sep[idx] == ',' && strings.HasPrefix(after, ":") {
			return 0, 0, false // that was a key, not a value
		}
		if sep[idx] == ',' && proseLike(in[vs:min(ve, vs+256)]) {
			return 0, 0, false // ["Authorization", "is a header"]
		}
	case kind != KindCredential && openQuote(in, nameS) != 0:
		// "Name: value" inside a string (JSON, curl -H "..."): the value runs to the string's end
		ve = scanQuoted(in, vs, openQuote(in, nameS), quoteEscapes(in, nameS))
		if kind == KindAuthHeader { // a stray quote before the name does not bound the value
			ve = max(ve, authValueEnd(in, vs))
		}
	case kind != KindCredential && headerLine(in, nameS):
		ve = vs + indexLineEnd(in[vs:])
		for { // obs-fold: continuation lines start with space or tab
			end, ok := foldAt(in, ve)
			if !ok {
				break
			}
			ve = end
		}
	case kind == KindCookie:
		ve = vs + indexLineEnd(in[vs:])
	case kind == KindAuthHeader:
		ve = authValueEnd(in, vs)
	default: // a credential: up to a blank, a double quote or a line break
		if sep[idx] == '=' && sepHasSpace(sep, idx) && (!lineStartExact(in, nameS) || strings.ContainsAny(prefix, "(){}")) {
			return 0, 0, false // password = getPassword(): code, not an ini line
		}
		for ve = vs; ve < len(in) && in[ve] != ' ' && in[ve] != '\t' && in[ve] != '"' && !lineBreakAt(in, ve) && !escapedQuoteAt(in, ve); ve++ {
		}
		if strings.IndexByte(in[vs:ve], '(') >= 0 {
			return 0, 0, false // password=getPassword(): a call
		}
	}
	return vs, ve, ve > vs && !skipHeaderValue(kind, in[vs:min(ve, vs+256)])
}

// foldAt reports whether the line break at i (CR LF or LF) is followed by a continuation line: one
// that starts with a blank and is not itself a "key:" line. It returns the end of that line.
func foldAt(in string, i int) (end int, ok bool) {
	if i < len(in) && in[i] == '\r' {
		i++
	}
	if i+1 >= len(in) || in[i] != '\n' || (in[i+1] != ' ' && in[i+1] != '\t') {
		return 0, false
	}
	next := skipSet(in, i+1, " \t", 1<<30)
	if startsKeyLine(in, next) {
		return 0, false
	}
	return next + indexLineEnd(in[next:]), true
}

// foldFollows reports whether a line break at i is followed by a continuation of the value.
func foldFollows(in string, i int) bool {
	_, ok := foldAt(in, i)
	return ok
}

// authValueEnd returns the end of an Authorization value that does not stand on a line of its
// own: a scheme and token, or a parameter list. The pattern is tried on the first KiB; a value
// that fills it is followed on to the next blank, quote or line break.
func authValueEnd(in string, vs int) int {
	seg := headerPrefix1K(in, vs)
	m := authValueRE.FindStringIndex(seg)
	if m == nil {
		return vs
	}
	ve := vs + m[1]
	if len(seg) == 1024 && m[1] >= 512 { // the window ended inside a parameter: resume at the last comma
		if c := strings.LastIndexByte(seg[:m[1]], ','); c > 0 {
			ve = vs + c
		}
	}
	for ve < len(in) && in[ve] == ',' { // a parameter list longer than the window goes on
		c := paramContRE.FindStringIndex(headerPrefix1K(in, ve))
		if c == nil {
			break
		}
		ve += c[1]
	}
	if m[1] == len(seg) {
		for ve < len(in) && in[ve] != ' ' && in[ve] != '\t' && in[ve] != '"' && in[ve] != '\'' && !lineBreakAt(in, ve) {
			ve++
		}
	}
	return ve
}

// scanQuoted returns the end of a quoted value that starts at vs: the matching closing quote q
// (k backslashes before it for an escaped quote), or the line break that ends the line.
func scanQuoted(in string, vs int, q byte, k int) int {
	closing := strings.Repeat("\\", k) + string(q)
	for i := vs; i < len(in); i++ {
		if lineBreakAt(in, i) {
			return i
		}
		if k == 0 {
			if in[i] == '\\' && i+1 < len(in) {
				i++
				continue
			}
			if in[i] == q {
				return i
			}
		} else if in[i] == '\\' && strings.HasPrefix(in[i:], closing) {
			return i
		}
	}
	return len(in)
}

func escapedQuoteAt(in string, i int) bool {
	return in[i] == '\\' && i+1 < len(in) && (in[i+1] == '"' || in[i+1] == '\'')
}

// lineStartExact reports whether the name starts a line with no indentation.
func lineStartExact(in string, nameS int) bool {
	return nameS == 0 || in[nameS-1] == '\n' || in[nameS-1] == '\r'
}

// proseLike reports a value of three or more lower-case words ("is a header").
func proseLike(v string) bool {
	f := strings.Fields(v)
	if len(f) < 3 {
		return false
	}
	for _, w := range f {
		for i := 0; i < len(w); i++ {
			if w[i] < 'a' || w[i] > 'z' {
				return false
			}
		}
	}
	return true
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

// headerPrefix returns up to 256 bytes of the text that starts at vs, cut at the first line break.
func headerPrefix(in string, vs int) string {
	seg := in[vs:min(len(in), vs+256)]
	return seg[:indexLineEnd(seg)]
}

// headerPrefix1K is the same for 1 KiB.
func headerPrefix1K(in string, vs int) string {
	seg := in[vs:min(len(in), vs+1024)]
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

// indexLineEnd returns the offset of the first line break in s, or len(s). A line break is CR, LF,
// or the two characters backslash-n or backslash-r, which is how one appears in escaped text.
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
		return len(v) < 4 || notAValue(v)
	}
	return false
}

// notAValue reports text that names a thing and is not a secret: a type or state word ("string",
// "expired"), or an identifier of letters, underscores and dots that reads as one: a field path
// ("resp.AccessToken"), snake_case, or camelCase ("tokenValue"). A plain lower-case word is not
// skipped (a weak password is still a password), and anything with a digit or a symbol never is.
func notAValue(v string) bool {
	switch strings.ToLower(v) {
	case "string", "number", "integer", "int", "bool", "boolean", "float", "double", "object", "array", "any",
		"null", "nil", "none", "true", "false", "undefined", "required", "optional", "expired", "invalid",
		"missing", "empty", "unknown", "redacted", "hidden", "masked", "secret", "password", "token":
		return true
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' })
	if len(parts) == 0 {
		return false
	}
	joined := len(parts) > 1 || len(parts[0]) != len(v)
	camel := false
	for _, p := range parts {
		switch kind := identifierPart(p); kind {
		case 0:
			return false
		case 2:
			camel = true
		}
	}
	return joined || camel
}

// identifierPart classifies a run of letters: 0 not an identifier word, 1 a lower-case word, 2
// camelCase words. In camelCase every upper-case letter starts a word of at least three letters
// and the words of four or more lower-case letters make up 80% of the run; random mixed-case
// letters fail that nearly always.
func identifierPart(p string) int {
	if p == "" {
		return 0
	}
	upper := false
	run, in := 0, 0
	for i := 0; i <= len(p); i++ {
		if i < len(p) && 'a' <= p[i] && p[i] <= 'z' {
			run++
			continue
		}
		if run >= 4 {
			in += run
		}
		if i < len(p) {
			if p[i] < 'A' || p[i] > 'Z' {
				return 0
			}
			upper = true
			if i+2 >= len(p) || p[i+1] < 'a' || p[i+1] > 'z' || p[i+2] < 'a' || p[i+2] > 'z' {
				return 0
			}
		}
		run = 0
	}
	if !upper {
		return 1
	}
	if in*10 < len(p)*8 {
		return 0
	}
	return 2
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
// The password runs to the last "@" before the next blank or quote, so a "/" inside it does not
// end it, unless what precedes that "/" is only digits (a port: host:8080/path@x).
func findUserinfo(in string, spans []span) []span {
	const stop = " \t\r\n\"'\\"
	for from := 0; ; {
		i := strings.Index(in[from:], "://")
		if i < 0 {
			return spans
		}
		j := from + i + 3
		from = j
		u := j
		for u < len(in) && in[u] != ':' && in[u] != '@' && in[u] != '/' && strings.IndexByte(stop, in[u]) < 0 {
			u++
		}
		if u == j || u >= len(in) || in[u] != ':' {
			continue
		}
		p := u + 1
		end := p
		for end < len(in) && strings.IndexByte(stop, in[end]) < 0 {
			end++
		}
		at := strings.LastIndexByte(in[p:end], '@')
		if at <= 0 {
			continue
		}
		pw := in[p : p+at]
		if slash := strings.IndexByte(pw, '/'); slash >= 0 {
			if allDigits(pw[:slash]) {
				continue
			}
		}
		spans = append(spans, span{p, p + at, KindCredential})
		from = p + at
	}
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// hasLongBase64Run reports whether in holds a run of at least n base64 characters.
func hasLongBase64Run(in string, n int) bool {
	run := 0
	for i := 0; i < len(in); i++ {
		c := in[i]
		if isAlnum(c) || c == '+' || c == '/' || c == '=' || c == '-' || c == '_' {
			if run++; run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// Fixed base64 forms of "-----BEGIN " and of {"auths" at the three byte alignments, so a PEM
// file or a Docker config embedded in base64 (a Kubernetes Secret's data) is found without decoding.
var (
	b64PEM    = [...]string{"LS0tLS1CRU", "0tLS0tQkVH", "tLS0tLUJFR"}
	b64Docker = [...]string{"eyJhdXRocy", "siYXV0aHMi", "7ImF1dGhzI"}
)

// findEncodedBlobs appends a span from each base64 PEM or docker-config prefix to the end of its
// base64 run.
func findEncodedBlobs(in string, spans []span) []span {
	if !hasLongBase64Run(in, 40) {
		return spans
	}
	for _, set := range []struct {
		prefixes []string
		kind     Kind
	}{{b64PEM[:], KindPrivateKey}, {b64Docker[:], KindCredential}} {
		for _, p := range set.prefixes {
			for from := 0; ; {
				i := strings.Index(in[from:], p)
				if i < 0 {
					break
				}
				a := from + i
				e := a + len(p)
				for e < len(in) && (isAlnum(in[e]) || in[e] == '+' || in[e] == '/' || in[e] == '=' || in[e] == '-' || in[e] == '_') {
					e++
				}
				spans = append(spans, span{a, e, set.kind})
				from = e
			}
		}
	}
	return spans
}
