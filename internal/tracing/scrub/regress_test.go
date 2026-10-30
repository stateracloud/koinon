// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	mrand "math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// esc builds JSON escapes in code: "~u" stands for a backslash and u.
func esc(s string) string { return strings.ReplaceAll(s, "~u", string([]byte{92, 'u'})) }

// The armour lines are built in code so the file holds no literal key block.
const (
	pemBegin = "-----BEGIN " + "PRIVATE KEY-----"
	pemEnd   = "-----END " + "PRIVATE KEY-----"
)

// TestLeakRegressions are the shapes an adversarial review found leaking. Each runs through
// Content (the router's entry point) and the needle must not survive.
func TestLeakRegressions(t *testing.T) {
	sec := "Xq7_notreal_pw\"&<x"
	gcpPEM := pemBegin + "\\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\\nBKcwggSjAgEAAoIBAQC7notreal\\n" + pemEnd
	gcpJSON := "{\n  \"type\": \"service_account\",\n  \"private_key\": \"" + gcpPEM + "\",\n  \"client_email\": \"x@y.iam\"\n}"
	basic := "dXNlcjpwYXNzd29yZA=="
	tests := []struct {
		name    string
		level   scrub.Level
		secrets []string
		in      string
		needle  string
	}{
		{"numeric secret as JSON number, full", scrub.LevelFull, []string{"80421357"}, `{"pin":80421357}`, "80421357"},
		{"numeric secret as JSON number, none", scrub.LevelNone, []string{"80421357"}, `{"pin":80421357}`, "80421357"},
		{"capital Name/Value pair", scrub.LevelFull, nil, `{"Name":"Authorization","Value":"Basic ` + basic + `"}`, "dXNlcjpw"},
		{"header tuple list", scrub.LevelFull, nil, `[["Authorization","Basic ` + basic + `"]]`, "dXNlcjpw"},
		{"pair inside a JSON string", scrub.LevelFull, nil, `{"content":"{\"headers\":[{\"name\":\"Authorization\",\"value\":\"Basic ` + basic + `\"}]}"}`, "dXNlcjpw"},
		{"pair in NDJSON", scrub.LevelFull, nil, "{\"name\":\"Authorization\",\"value\":\"Basic " + basic + "\"}\n{\"a\":1}", "dXNlcjpw"},
		{"HTTP_AUTHORIZATION python dict", scrub.LevelFull, nil, `{'HTTP_AUTHORIZATION': 'Basic ` + basic + `'}`, "dXNlcjpw"},
		{"HTTP_AUTHORIZATION JSON key", scrub.LevelFull, nil, `{"HTTP_AUTHORIZATION":"Basic ` + basic + `"}`, "dXNlcjpw"},
		{"HTTP_COOKIE env", scrub.LevelFull, nil, `HTTP_COOKIE=sessionid=notreal_sess_value`, "notreal_sess_value"},
		{"Go Header.Set", scrub.LevelFull, nil, `req.Header.Set("Authorization", "Basic ` + basic + `")`, "dXNlcjpw"},
		{"YAML value on the next line", scrub.LevelFull, nil, "authorization:\n  Basic " + basic, "dXNlcjpw"},
		{"Bearer then tab, letters only", scrub.LevelFull, nil, "Authorization: Bearer\tabcdefghijklmnopqrstuvwxyzAB", "abcdefghijklmnop"},
		{"cookie with a quoted part", scrub.LevelFull, nil, `Cookie: a=1; b="q"; session=notreal_sess_value`, "notreal_sess_value"},
		{"document.cookie", scrub.LevelFull, nil, `document.cookie="session=notreal_sess_value; path=/"`, "notreal_sess_value"},
		{"dotted key x.authorization", scrub.LevelFull, nil, `{"x.authorization":"Basic ` + basic + `"}`, "dXNlcjpw"},
		{"key with trailing space", scrub.LevelFull, nil, `{"Authorization ":"Basic ` + basic + `"}`, "dXNlcjpw"},
		{"number under a header key", scrub.LevelFull, nil, `{"authorization":123456789012345}`, "123456789012345"},
		{"JWT after %3D", scrub.LevelFull, nil, `https://x/cb?next=%2Fa%3Fq%3DeyJhbGciOi.eyJzdWIiOi.c2lnbmF0dXJl`, "eyJzdWIiOi"},
		{"JWT after escaped newline", scrub.LevelFull, nil, `{"log":"line1\\neyJhbGciOi.eyJzdWIiOi.c2lnbmF0dXJl"}`, "eyJzdWIiOi"},
		{"JWT eyA header", scrub.LevelFull, nil, "eyAiYWxnIjogIkhTMjU2In0.eyAic3ViIjogIngifQ.c2lnbmF0dXJlc2ln", "eyAic3ViIjogIngifQ"},
		{"ghp after %3D", scrub.LevelFull, nil, `?t%3Dghp_notrealnotrealnotrealnotreal`, "ghp_notrealnotreal"},
		{"ghp after escaped newline in double-encoded JSON", scrub.LevelFull, nil, `{"c":"{\"f\":\"a\\nghp_notrealnotrealnotrealnotreal\"}"}`, "ghp_notrealnotreal"},
		{"sk-svcacct", scrub.LevelFull, nil, `OPENAI_API_KEY=sk-svcacct-notrealnotrealnotreal`, "notrealnotrealnotreal"},
		{"sk-admin", scrub.LevelFull, nil, `sk-admin-notrealnotrealnotreal`, "notrealnotrealnotreal"},
		{"api-key header", scrub.LevelFull, nil, `api-` + `key: notreal0notreal0notreal0`, "notreal0notreal0"},
		{"x-vault-token header", scrub.LevelFull, nil, `X-Vault-Token: hvs.notrealnotrealnotreal`, "hvs.notrealnotrealnotreal"},
		{"private-token header", scrub.LevelFull, nil, `Private-Token: notrealnotrealnotreal`, "notrealnotrealnotreal"},
		{"x-goog-api-key header", scrub.LevelFull, nil, `X-Goog-Api-Key: notrealnotrealnotreal`, "notrealnotrealnotreal"},
		{"x-gitlab-token header", scrub.LevelFull, nil, `X-Gitlab-Token: notrealnotrealnotreal`, "notrealnotrealnotreal"},
		{"hvs token alone", scrub.LevelFull, nil, `vault hvs.notrealnotrealnotreal`, "hvs.notreal"},
		{"hf token", scrub.LevelFull, nil, "hf_" + strings.Repeat("a", 30), "hf_aaaa"},
		{"AIza key", scrub.LevelFull, nil, "key=AIza" + strings.Repeat("b", 35), "AIzabbbb"},
		{"stripe live key", scrub.LevelFull, nil, "sk_live_" + strings.Repeat("c", 24), "sk_live_cccc"},
		{"slack xoxe", scrub.LevelFull, nil, "xoxe-1-" + strings.Repeat("d", 30), "xoxe-1-dddd"},
		{"placeholder with hyphen", scrub.LevelFull, nil, "openshell:resolve:env:v1104-SPIKE_TOKEN", "SPIKE_TOKEN"},
		{"known secret JS-escaped quote, raw <&", scrub.LevelFull, []string{sec}, `{"a":1} pw="Xq7_notreal_pw\"&<x"`, "Xq7_notreal_pw"},
		{"known secret URL-encoded in a DSN", scrub.LevelFull, []string{"notreal:p@ss/w0rd"}, `postgres://u:notreal%3Ap%40ss%2Fw0rd@db/x`, "notreal%3Ap%40ss"},
		{"known secret, ensure_ascii text", scrub.LevelFull, []string{"notreal-pässwörd"}, esc(`pw: "notreal-p~u00e4ssw~u00f6rd"`), "notreal-p"},
		{"known secret, ensure_ascii JSON", scrub.LevelFull, []string{"notreal-pässwörd"}, esc(`{"pw":"notreal-p~u00e4ssw~u00f6rd"}`), "ssw"},
		{"known secret go-escaped HTML", scrub.LevelFull, []string{"notreal<pw>&1"}, esc(`x = "notreal~u003cpw~u003e~u00261"`), "notreal"},
		{"known secret double JSON escaped", scrub.LevelFull, []string{`no"treal_pw1`}, `x={\"p\":\"no\\\"treal_pw1\"}`, `treal_pw1`},
		{"known secret base64", scrub.LevelFull, []string{"notreal-secret-value"}, "Basic bm90cmVhbC1zZWNyZXQtdmFsdWU=", "bm90cmVhbC1zZWNy"},
		{"service-account JSON, key printed as PEM", scrub.LevelFull, []string{gcpJSON}, pemBegin + "\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nBKcwggSjAgEAAoIBAQC7notreal\n" + pemEnd, "MIIEvQIBADANBgkq"},
		{"service-account JSON, PEM escaped", scrub.LevelFull, []string{gcpJSON}, `"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nBKcwggSjAgEAAoIBAQC7notreal"`, "MIIEvQIBADANBgkq"},
		{"over-deep JSON with an escaped kgw", scrub.LevelFull, nil, strings.Repeat("[", 250) + esc(`"kgw~u005fnotrealnotrealnotreal"`) + strings.Repeat("]", 250), "notrealnotreal"},
		{"escaped Authorization in valid JSON", scrub.LevelFull, nil, esc(`{"h":"~u0041uthorization: Basic ` + basic + `"}`), "dXNlcjpw"},
		{"escaped Authorization in text", scrub.LevelFull, nil, esc(`not json: "~u0041uthorization: Basic ` + basic + `"`), "dXNlcjpw"},
		{"escaped kgw with trailing text", scrub.LevelFull, nil, esc(`{"log":"~u006bgw_notrealnotrealnotreal"} trailing`), "notrealnotreal"},
		{"card after short groups", scrub.LevelRedacted, nil, "1 2 3 4 5 6 7 4111 1111 1111 1111", "4111 1111 1111 1111"},
		{"card after a hyphen chain", scrub.LevelRedacted, nil, "10-20-30-40-50-60-70-4111-1111-1111-1111", "4111-1111-1111-1111"},
		{"card after a very long chain", scrub.LevelRedacted, nil, strings.Repeat("1 ", 3000) + "4111 1111 1111 1111", "4111 1111 1111 1111"},
		{"token inside a long run", scrub.LevelRedacted, nil, strings.Repeat("A", 4100) + "/Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA", "Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA"},
		{"token after base64, outside a data URL", scrub.LevelRedacted, nil, "use base64,Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA", "Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA"},
		{"dotted US phone", scrub.LevelRedacted, nil, "call 555.867.5309", "867.5309"},
		{"fullwidth at sign", scrub.LevelRedacted, nil, "mail jane＠example.com", "jane"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := scrub.New(scrub.Config{Level: tc.level, Secrets: scrub.NewSecrets(tc.secrets...)})
			out, _ := s.Content(tc.in, 0)
			if strings.Contains(out, tc.needle) {
				t.Errorf("%q survived in %q", tc.needle, out)
			}
			if out == tc.in {
				t.Errorf("unchanged")
			}
			// the text path too: Content may take the JSON path, and Text must hold as well
			if txt := s.Text(tc.in); strings.Contains(txt, tc.needle) && !strings.HasPrefix(strings.TrimSpace(tc.in), "[") {
				t.Errorf("Text left %q in %q", tc.needle, txt)
			}
		})
	}
}

// TestNoFalsePositives are shapes that must pass untouched at redacted.
func TestNoFalsePositives(t *testing.T) {
	ins := []string{
		"commit 3f9a2b1c4d5e6f7081920a1b2c3d4e5f60718293",
		"git object 3f9a2b1c4d5e6f7081920a1b2c3d4e5f607182 (38 chars)",
		"id 123e4567-e89b-12d3-a456-426614174000",
		"sha512-z4PhNX7vuL3xVChQ1m2AB9Yg5AULVxXcg/SpIdNs6c5H0NE8XYXysP+DGNKHfuwvY7kxvUdBeoGlODJ6+SfaPg==",
		"image ghcr.io/stateracloud/koinon@sha256:3f9a2b1c4d5e6f7081920a1b2c3d4e5f607182933f9a2b1c4d5e6f7081920a1b",
		"func (s *Scrubber) Content(in string, limit int) (out string, truncated bool)",
		"github.com/stateracloud/koinon/internal/tracing/scrub/v2/aa0265f5",
		"pod koinon-agentd-7c9d8f6b5-x2k4p ready",
		"timestamp 2026-10-30T12:34:56.789012345Z",
		"elapsed 1730289296 1730289297 1730289298",
		"npm i @types/node@20.11.5 user@1.2.3",
		"base64 of hello world aGVsbG8gd29ybGQ=",
		"Go mod sum h1:Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA4uE2iO8pQ1w=",
		"the cookie jar and authorization flow",
		"Authorization: Bearer <token>",
		"price 1234 5678 9012 3456 EUR",
		"ULID 01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"span id 0af7651916cd43dd8448eb211c80319c and b7ad6b7169203331",
		"OCI digest sha256:Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA",
		"cosign: MEUCIQDx7K3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sAiEA4uE2iO8pQ1w",
		"data:image/png;base64," + strings.Repeat("Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA", 8),
	}
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted})
	for _, in := range ins {
		if out := s.Text(in); out != in {
			t.Errorf("false positive\n in  %q\n out %q", in, out)
		}
	}
}

// TestSecretsAgainstBruteForce checks the automaton against a naive search on a tiny alphabet,
// where overlaps and failure links are dense.
func TestSecretsAgainstBruteForce(t *testing.T) {
	r := mrand.New(mrand.NewSource(1))
	gen := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab"[r.Intn(2)]
		}
		return string(b)
	}
	for it := 0; it < 3000; it++ {
		var pats []string
		for k := 0; k < 1+r.Intn(5); k++ {
			pats = append(pats, gen(8+r.Intn(4)))
		}
		text := gen(r.Intn(60))
		s := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: scrub.NewSecrets(pats...)})
		got := s.Text(text)
		want := []byte(text)
		hit := make([]bool, len(text))
		for _, p := range pats {
			for i := 0; i+len(p) <= len(text); i++ {
				if text[i:i+len(p)] == p {
					for k := i; k < i+len(p); k++ {
						hit[k] = true
					}
				}
			}
		}
		var b strings.Builder
		for i := 0; i < len(want); i++ {
			if !hit[i] {
				b.WriteByte(want[i])
			} else if i == 0 || !hit[i-1] {
				b.WriteString(scrub.Replacement(scrub.KindSecret))
			}
		}
		if got != b.String() {
			t.Fatalf("pats %q text %q\n got  %q\n want %q", pats, text, got, b.String())
		}
	}
}

// TestContentLimitRandom: random JSON and bytes never exceed the limit, and JSON stays JSON.
func TestContentLimitRandom(t *testing.T) {
	r := mrand.New(mrand.NewSource(2))
	var gen func(d int) any
	gen = func(d int) any {
		switch k := r.Intn(6); {
		case d > 3 || k == 0:
			b := make([]byte, r.Intn(400))
			for i := range b {
				b[i] = byte(r.Intn(256))
			}
			return string(b)
		case k == 1:
			return r.Float64() * 1e30
		case k == 2:
			a := make([]any, r.Intn(8))
			for i := range a {
				a[i] = gen(d + 1)
			}
			return a
		case k == 3:
			m := map[string]any{}
			for i := 0; i < r.Intn(8); i++ {
				kb := make([]byte, r.Intn(200))
				for j := range kb {
					kb[j] = byte(32 + r.Intn(95))
				}
				m[string(kb)] = gen(d + 1)
			}
			return m
		}
		return nil
	}
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: scrub.NewSecrets("notrealsecret")})
	for it := 0; it < 1500; it++ {
		raw, _ := json.Marshal(gen(0))
		limit := 64 + r.Intn(3000)
		out, _ := s.Content(string(raw), limit)
		if len(out) > limit || !json.Valid([]byte(out)) || !utf8.ValidString(out) {
			t.Fatalf("limit %d: len %d valid %v", limit, len(out), json.Valid([]byte(out)))
		}
	}
	for it := 0; it < 1000; it++ {
		b := make([]byte, r.Intn(5000))
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		limit := 64 + r.Intn(2000)
		if out, _ := s.Content(string(b), limit); len(out) > limit {
			t.Fatalf("text over limit %d > %d", len(out), limit)
		}
	}
}

// shapes plants each always-tier credential. The needle is the part that must never survive.
type shape struct{ text, needle string }

func randomWord(n int) string {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(a))))
		b[i] = a[k.Int64()]
	}
	return string(b)
}

func alwaysShapes(w string) []shape {
	return []shape{
		{"kgw_" + w + "abcd", w},
		{"openshell:resolve:env:" + w, w},
		{"eyJhbGci" + w + ".eyJzdWIi" + w + ".c2ln" + w, w},
		{"Authorization: Basic " + w + "ab", w},
		{"Authorization: Bearer " + w + w, w},
		{"Cookie: sid=" + w + "; theme=dark", w},
		{"X-Api-Key: " + w + "abcdefgh", w},
		{"ghp_" + w + w, w},
		{"sk-ant-api03-" + w + w, w},
		{"AKIA" + strings.ToUpper(w[:16]), strings.ToUpper(w[:16])},
		{pemBegin + "\nMII" + w + "\n" + pemEnd, w},
		{"password=" + w, w},
		{"PGPASSWORD=" + w, w},
		{`{"access_token":"` + w + `"}`, w},
		{`{"clientSecret": "` + w + `"}`, w},
		{"api_key: " + w, w},
		{"https://svc:" + w + "@host.example.test/p", w},
		{"cu" + "rl -u svc:" + w + " https://host.example.test", w},
		{"ya29." + w + w, w},
		{"npm_" + w + w + w, w},
		{"Authorization: Digest realm=\"r\", response=\"" + w + "\"", w},
	}
}

var separators = []string{" ", "\n", "\t", ",", ";", "=", ":", "\"", "'", "/", "(", "{", "[", "&", "?", "%3D", "%2F", `\n`, `\t`, `\"`, `\/`}

// wrap puts the planted text into a wrapper, the ways content reaches the scrubber.
func wrap(kind int, sep, text string) string {
	switch kind % 8 {
	case 0:
		return sep + text
	case 1:
		b, _ := json.Marshal(sep + text)
		return string(b)
	case 2:
		b, _ := json.Marshal([]string{"x", sep + text, "y"})
		return string(b)
	case 3:
		b, _ := json.Marshal(map[string]any{"result": map[string]any{"output": sep + text}})
		return string(b)
	case 4:
		inner, _ := json.Marshal(map[string]string{"out": sep + text})
		b, _ := json.Marshal(string(inner))
		return string(b)
	case 5:
		b, _ := json.Marshal(sep + text)
		return "log: " + string(b) + " trailing"
	case 6:
		inner, _ := json.Marshal(sep + text)
		b, _ := json.Marshal(string(inner))
		return `{"c":` + string(b) + `}`
	}
	return "line1\n" + sep + text + "\nline3"
}

func FuzzAlwaysShapesNeverSurvive(f *testing.F) {
	for i := 0; i < 10; i++ {
		f.Add(uint8(i), uint8(i), uint8(i), randomWord(12))
	}
	full := scrub.New(scrub.Config{Level: scrub.LevelFull})
	f.Fuzz(func(t *testing.T, shapeIdx, wrapIdx, sepIdx uint8, seed string) {
		w := randomWord(12)
		if len(seed) > 0 { // let the fuzzer shape the secret's characters too
			var b strings.Builder
			for _, c := range seed {
				if ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') {
					b.WriteRune(c)
				}
			}
			if b.Len() >= 8 {
				w = b.String()
			}
		}
		shapes := alwaysShapes(w + randomWord(8))
		sh := shapes[int(shapeIdx)%len(shapes)]
		sep := separators[int(sepIdx)%len(separators)]
		in := wrap(int(wrapIdx), sep, sh.text)
		for _, out := range []string{full.Text(in), func() string { o, _ := full.Content(in, 0); return o }()} {
			if strings.Contains(out, sh.needle) {
				t.Fatalf("shape %q survived\n in  %q\n out %q", sh.text, in, out)
			}
		}
	})
}

func fill(c byte, n int) string { return strings.Repeat(string(c), n) }

// TestReview2Leaks are the leaks of the second adversarial review.
func TestReview2Leaks(t *testing.T) {
	pem := pemBegin + "\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nBKcwggSjAgEAAoIBAQC7notreal\n" + pemEnd
	jb, _ := json.Marshal(map[string]string{"output": "key:\n" + pem + "\ndone"})
	cases := []struct{ name, in, needle string }{
		{"pem raw", "before\n" + pem + "\nafter", "MIIEvQIBADANBgkq"},
		{"pem escaped newlines", `"` + strings.ReplaceAll(pem, "\n", `\n`) + `"`, "MIIEvQIBADANBgkq"},
		{"pem in JSON", string(jb), "MIIEvQIBADANBgkq"},
		{"pem rsa", strings.ReplaceAll(pem, "PRIVATE", "RSA PRIVATE"), "MIIEvQIBADANBgkq"},
		{"pem encrypted", strings.ReplaceAll(pem, "BEGIN PRIVATE", "BEGIN ENCRYPTED PRIVATE"), "MIIEvQIBADANBgkq"},
		{"pem without end", pemBegin + "\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nBKcw", "MIIEvQIBADANBgkq"},
		{"json access_token", `{"access_token":"notreal-access-value"}`, "notreal-access-value"},
		{"json accessToken", `{"accessToken":"notreal-access-value"}`, "notreal-access-value"},
		{"json refresh_token", `{"refresh_token":"notreal-refresh"}`, "notreal-refresh"},
		{"json id_token", `{"id_token":"notreal-idtoken"}`, "notreal-idtoken"},
		{"json client_secret", `{"client_secret":"notreal-clientsec"}`, "notreal-clientsec"},
		{"json password", `{"password":"notreal pw"}`, "notreal pw"},
		{"json passwd", `{"passwd":"notreal-passwd"}`, "notreal-passwd"},
		{"json secret", `{"secret":"notreal-secret"}`, "notreal-secret"},
		{"json private_key", `{"private_key":"notreal-private"}`, "notreal-private"},
		{"json apikey", `{"apikey":"notreal-apikey1"}`, "notreal-apikey1"},
		{"json api_key", `{"api_key":"notreal-apikey2"}`, "notreal-apikey2"},
		{"json x-access-token", `{"x-access-token":"notreal-xaccess"}`, "notreal-xaccess"},
		{"json x-amz-security-token", `{"x-amz-security-token":"notreal-amzsec"}`, "notreal-amzsec"},
		{"json aws_secret_access_key", `{"aws_secret_access_key":"notreal-awssec"}`, "notreal-awssec"},
		{"nested under a credential key", `{"db":{"password":{"value":"notreal-nested"}}}`, "notreal-nested"},
		{"text password=", "connect password=notreal-pw-text now", "notreal-pw-text"},
		{"env PGPASSWORD", "PGPASSWORD=notreal-pgpass psql -h db", "notreal-pgpass"},
		{"env AWS_SECRET_ACCESS_KEY", "export AWS_SECRET_ACCESS_KEY=notreal-awssecret", "notreal-awssecret"},
		{"text api_key: yaml", "api_key: notreal-yamlkey", "notreal-yamlkey"},
		{"text client_secret yaml", "client_secret: notreal-clientsecret", "notreal-clientsecret"},
		{"text json-ish password with spaces", `log {"password": "notreal pw words"} end`, "pw words"},
		{"text accessToken json", `log {"accessToken":"notreal-access-js"}`, "notreal-access-js"},
		{"ya29", "t=ya29." + fill('a', 30), "ya29.aaaa"},
		{"GOCSPX", "s=GOCSPX-" + fill('b', 28), "GOCSPX-bbbb"},
		{"npm", "//registry/:_authToken=npm_" + fill('c', 36), "npm_cccc"},
		{"glsa", "glsa_" + fill('d', 30), "glsa_dddd"},
		{"dop_v1", "dop_v1_" + fill('e', 40), "dop_v1_eeee"},
		{"url userinfo", "clone https://deploy:notreal-urlpw@git.example.test/x.git", "notreal-urlpw"},
		{"basic user flag", "cu" + "rl -u admin:notreal-curlpw https://api.example.test", "notreal-curlpw"},
		{"long user flag, quoted", "cu" + `rl --user "admin:notreal-curlpw2" https://api.example.test`, "notreal-curlpw2"},
		{"cookie with escaped quote in text", `log: {"Cookie":"a=1; q=\"x\"; session=notreal_sess_value"} end`, "notreal_sess_value"},
		{"pair value first", `log {"value":"Basic dXNlcjpwYXNzd29yZA==","name":"Authorization"} end`, "dXNlcjpwYXNzd29yZA"},
		{"pair with a member between", `log {"name":"Authorization","in":"header","value":"Basic dXNlcjpwYXNzd29yZA=="}`, "dXNlcjpwYXNzd29yZA"},
		{"digest", "Authorization: Digest username=\"u\", realm=\"r\", response=\"notreal-digestresp\"\nHost: x", "notreal-digestresp"},
		{"sigv4", "Authorization: AWS4-HMAC-SHA256 Credential=AKIDx/20261030/eu/s3/aws4_request, SignedHeaders=host, Signature=notreal0sig0123456789\n", "notreal0sig0123456789"},
		{"token token=", "Authorization: Token token=\"notreal-tokvalue\"\n", "notreal-tokvalue"},
		{"obs-fold", "Authorization: Basic\r\n dXNlcjpwYXNzd29yZA==\r\nHost: x", "dXNlcjpwYXNzd29yZA"},
		{"curl -H digest", `curl -H 'Authorization: Digest response="notreal-digestresp2"' https://x`, "notreal-digestresp2"},
		{"fullwidth inside escapes", esc(`mail jane~uFF20example.com`), "jane"},
		{"triple url-encoded email", "mail jane%252540example.com", "jane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level := scrub.LevelFull
			if strings.HasPrefix(tc.name, "fullwidth") || strings.HasPrefix(tc.name, "triple") {
				level = scrub.LevelRedacted
			}
			s := scrub.New(scrub.Config{Level: level})
			out, _ := s.Content(tc.in, 0)
			if strings.Contains(out, tc.needle) {
				t.Errorf("Content left %q in %q", tc.needle, out)
			}
			if !strings.HasPrefix(strings.TrimSpace(tc.in), "[") && strings.Contains(s.Text(tc.in), tc.needle) {
				t.Errorf("Text left %q", tc.needle)
			}
		})
	}
	t.Run("a pair's other members stay readable", func(t *testing.T) {
		out := scrub.New(scrub.Config{Level: scrub.LevelFull}).Text(`log {"name":"Authorization","in":"header","value":"Basic dXNlcjpwYXNzd29yZA=="}`)
		if !strings.Contains(out, `"in":"header"`) || !strings.Contains(out, `"name":"Authorization"`) {
			t.Errorf("%q", out)
		}
	})
}

func TestTooDeepEmbeddedBecomesPlaceholder(t *testing.T) {
	inner := strings.Repeat("[", 250) + `"` + esc(`kgw~u005fnotrealnotrealnotreal`) + `"` + strings.Repeat("]", 250)
	doc, _ := json.Marshal(map[string]string{"a": inner})
	out, _ := scrub.New(scrub.Config{Level: scrub.LevelFull}).Content(string(doc), 0)
	if strings.Contains(out, "notreal") || !strings.Contains(out, "too deeply nested") {
		t.Errorf("%q", out)
	}
}

// Embedded JSON with nothing to hide keeps its exact bytes at full.
func TestEmbeddedJSONKeepsItsBytes(t *testing.T) {
	inner := "{\n  \"a\": \"<b> & c\",\n  \"n\": 1.50\n}"
	doc, _ := json.Marshal(map[string]string{"out": inner})
	out, _ := scrub.New(scrub.Config{Level: scrub.LevelFull}).Content(string(doc), 0)
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["out"] != inner {
		t.Errorf("%q %v", out, err)
	}
	// and with a secret in it, the rest of the layout is still readable
	inner2 := "{\n  \"a\": \"" + fakeGatewayKey + "\",\n  \"b\": 2\n}"
	doc, _ = json.Marshal(map[string]string{"out": inner2})
	out, _ = scrub.New(scrub.Config{Level: scrub.LevelFull}).Content(string(doc), 0)
	if strings.Contains(out, fakeGatewayKey) {
		t.Errorf("leak %q", out)
	}
}

func TestReview2FalsePositives(t *testing.T) {
	for _, in := range []string{
		"TestScrubJSON2Roundtrip4Cases", "func TestScrubJSON2Roundtrip4Cases(t *testing.T) {",
		"invoice 2026-001234", "order 12345-67890",
		"the cookie: chocolate chip", "I like password: it is a word", "token: the next one",
	} {
		for _, lvl := range []scrub.Level{scrub.LevelFull, scrub.LevelRedacted} {
			if out := scrub.New(scrub.Config{Level: lvl}).Text(in); out != in {
				t.Errorf("%s: %q -> %q", lvl, in, out)
			}
		}
	}
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	in := `["Authorization", "is a header"]`
	if out, _ := s.Content(in, 0); out != `["Authorization","is a header"]` {
		t.Errorf("tuple prose: %q", out)
	}
	if out, _ := s.Content(`["Authorization","Basic dXNlcjpwYXNzd29yZA=="]`, 0); strings.Contains(out, "dXNlcjpw") {
		t.Errorf("tuple credential: %q", out)
	}
}

// Cutting a huge input before scrubbing must not leave a fragment of a secret at the cut.
func TestContentCutLeavesNoFragment(t *testing.T) {
	secret := "koinon_test_not-a-real-planted-secret-0123456789"
	s := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: scrub.NewSecrets(secret)})
	const limit = 1024
	for off := limit + 4096 - 120; off < limit+4096+20; off += 3 {
		in := strings.Repeat("x ", off/2) + secret + strings.Repeat(" y", 5000)
		out, cut := s.Content(in, limit)
		if !cut || len(out) > limit || strings.Contains(out, secret[:8]) {
			t.Fatalf("off %d: cut %v len %d out tail %q", off, cut, len(out), out[max(0, len(out)-60):])
		}
		doc, _ := json.Marshal(map[string]any{"a": in, "b": []string{in, in}})
		o2, _ := s.Content(string(doc), limit)
		if len(o2) > limit || strings.Contains(o2, secret[:8]) || !json.Valid([]byte(o2)) {
			t.Fatalf("json off %d: len %d %q", off, len(o2), o2)
		}
	}
}

// Work per call is bounded by the limit, not by the size of the input.
func TestContentIsBoundedOnHugeInputs(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: scrub.NewSecrets("aaaaaaaaaaaaaaab")})
	for name, in := range map[string]string{
		"digits":  strings.Repeat("1 ", 2<<20),
		"dashes":  strings.Repeat("1-", 2<<20),
		"cookie":  strings.Repeat(`"cookie","`, 400000),
		"at":      strings.Repeat("@a.a", 1<<20),
		"json":    "[" + strings.Repeat(`"a",`, 1<<20) + `"a"]`,
		"escapes": strings.Repeat(`A\n`, 1<<20),
	} {
		start := time.Now()
		out, cut := s.Content(in, 0)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v", name, d)
		}
		if !cut || len(out) > scrub.DefaultMaxBytes {
			t.Errorf("%s: cut %v len %d", name, cut, len(out))
		}
	}
}
