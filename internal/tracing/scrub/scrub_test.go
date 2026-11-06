// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// Fixtures are fake on purpose (AGENTS.md): they match a pattern and look nothing like a real
// credential.
const (
	fakeGatewayKey = "kgw_koinon_test_not-a-real-key"
	fakeJWT        = "eyJhbGci.eyJzdWIi.c2ln"
	fakeBlob       = "Zk3pQ9vL2xT7mW4bN8cR5dY1hG6jF0sA" // high entropy, meaningless
)

func redacted(k scrub.Kind) string { return scrub.Replacement(k) }

func TestGatewayKeyPrefixMatchesGatewayPackage(t *testing.T) {
	if !strings.HasPrefix(fakeGatewayKey, gateway.KeyPrefix) {
		t.Fatalf("scrub's kgw_ rule and gateway.KeyPrefix %q disagree", gateway.KeyPrefix)
	}
	k, err := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "bot"})
	if err != nil {
		t.Skipf("cannot issue a key: %v", err)
	}
	got := scrub.New(scrub.Config{Level: scrub.LevelFull}).Text("key " + k.Material + " end")
	if got != "key "+redacted(scrub.KindGatewayKey)+" end" {
		t.Errorf("a real issued key survived: %q", got)
	}
}

// cases: text, scrubbed at each level. A level whose want is the same as the input is a pass-through.
func TestText(t *testing.T) {
	always := func(in, want string) (string, string, string) { return in, want, want }
	tests := []struct {
		name string
		in   string
		full string // want at LevelFull and LevelNone
		red  string // want at LevelRedacted
	}{
		// always rules
		{"gateway key", "use " + fakeGatewayKey + " now", "use " + redacted(scrub.KindGatewayKey) + " now", ""},
		{"placeholder", "OPENAI_API_KEY=openshell:resolve:env:KOINON_GATEWAY_KEY", "OPENAI_API_KEY=" + redacted(scrub.KindPlaceholder), ""},
		{"jwt", "passport " + fakeJWT + ".", "passport " + redacted(scrub.KindJWT) + ".", ""},
		{"authorization header", "Authorization: Basic dXNlcjpwYXNz\nHost: x", "Authorization: " + redacted(scrub.KindAuthHeader) + "\nHost: x", ""},
		{"authorization json", `{"Authorization": "Bearer abc"}`, `{"Authorization": "` + redacted(scrub.KindAuthHeader) + `"}`, ""},
		{"authorization escaped json", `{\"authorization\":\"Token abc\"}`, `{\"authorization\":\"` + redacted(scrub.KindAuthHeader) + `\"}`, ""},
		{"proxy authorization", "proxy-authorization: Negotiate YII", "proxy-authorization: " + redacted(scrub.KindAuthHeader), ""},
		{"cookie", "Cookie: sid=abc; theme=dark\n", "Cookie: " + redacted(scrub.KindCookie) + "\n", ""},
		{"set-cookie", "Set-Cookie: sid=abc; HttpOnly", "Set-Cookie: " + redacted(scrub.KindCookie), ""},
		{"bearer", "got Bearer abcdefghijklmnop1234 back", "got " + redacted(scrub.KindBearer) + " back", ""},
		{"bearer in prose", "the bearer of bad news", "the bearer of bad news", "the bearer of bad news"},
		{"anthropic key", "sk-ant-api03-" + fk(93) + "AA", redacted(scrub.KindAPIKey), ""},
		{"anthropic key, truncated", "sk-ant-api03-" + fk(20), redacted(scrub.KindAPIKey), ""},
		{"github token", "ghp_" + fk(36), redacted(scrub.KindAPIKey), ""},
		{"aws id", "id AKIA" + fake(fakeUpper, 16, 0) + " ok", "id " + redacted(scrub.KindAPIKey) + " ok", ""},
		{"two in a row", fakeGatewayKey + " " + fakeJWT, redacted(scrub.KindGatewayKey) + " " + redacted(scrub.KindJWT), ""},

		// redacted-only rules
		{"email", "mail jane.doe@example.com please", "", "mail " + redacted(scrub.KindEmail) + " please"},
		{"email plus tag", "a+b@mail.example.co.uk.", "", redacted(scrub.KindEmail) + "."},
		{"phone international", "call +1 (555) 010-0199 today", "", "call " + redacted(scrub.KindPhone) + " today"},
		{"phone e164", "tel +31612345678", "", "tel " + redacted(scrub.KindPhone)},
		{"phone national", "ring 020 7946 0958 now", "", "ring " + redacted(scrub.KindPhone) + " now"},
		{"iban compact", "pay DE89370400440532013000 now", "", "pay " + redacted(scrub.KindIBAN) + " now"},
		{"iban grouped", "IBAN: GB82 WEST 1234 5698 7654 32.", "", "IBAN: " + redacted(scrub.KindIBAN) + "."},
		{"card spaced", "card 4111 1111 1111 1111 exp", "", "card " + redacted(scrub.KindCard) + " exp"},
		{"card hyphen", "5500-0000-0000-0004", "", redacted(scrub.KindCard)},
		{"card amex", "340000000000009", "", redacted(scrub.KindCard)},
		{"high entropy", "value " + fakeBlob + " end", "", "value " + redacted(scrub.KindToken) + " end"},

		// the same text at every level: no match
		{"plain prose", "Fix the failing test in internal/tracing and rerun make test.", "", ""},
		{"email-looking npm", "npm i @scope/pkg@1.2.3 left@right", "", ""},
		{"bad iban checksum", "DE89370400440532013001", "", ""},
		{"bad luhn", "4111 1111 1111 1112", "", ""},
		{"long id passing luhn but no issuer", "0000000000000000", "", ""},
		{"short number", "port 8080 and 443, 12345678", "", ""},
		{"date", "on 2026-10-30 and 30.10.2026 and 10/30/2026", "", ""},
		{"ip and version", "from 192.168.100.200 running v10.20.30.40", "", ""},
		{"thousands", "total 1 000 000 000 units", "", ""},
		{"range", "lines 100-200-300", "", ""},
		{"timestamp ns", "ts 1730000000000000000 done", "", ""},
		{"uuid", "id 3f2b8c1e-9a4d-4e7b-8c21-5d6f7a8b9c0d ok", "", ""},
		{"git sha1", "commit 3f2b8c1e9a4d4e7b8c215d6f7a8b9c0d1e2f3a4b", "", ""},
		{"git sha256", "3f2b8c1e9a4d4e7b8c215d6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b", "", ""},
		{"sri hash", "sha512-" + strings.Repeat("Ab1+", 22) + "==", "", ""},
		{"path", "internal/tracing/scrub/scrub_test.go and /usr/local/lib/python3.11/site-packages/foo", "", ""},
		{"url with query", "https://example.com/search?q=hello+world&page=2&session=42&sort=desc#top", "", ""},
		{"go identifiers", "func TestProviderRefusalHandlerFactoryImplementation123() {}", "", ""},
		{"image data url", "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ", 3), "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			full, red := tc.full, tc.red
			if full == "" {
				full = tc.in
			}
			if red == "" {
				red = full
				if tc.red == "" && tc.full == "" {
					red = tc.in
				}
			}
			for _, lvl := range []scrub.Level{scrub.LevelNone, scrub.LevelFull} {
				if got := scrub.New(scrub.Config{Level: lvl}).Text(tc.in); got != full {
					t.Errorf("level %s:\n got %q\nwant %q", lvl, got, full)
				}
			}
			if got := scrub.New(scrub.Config{Level: scrub.LevelRedacted}).Text(tc.in); got != red {
				t.Errorf("level redacted:\n got %q\nwant %q", got, red)
			}
		})
	}
	_ = always
}

func TestKnownSecrets(t *testing.T) {
	secrets := scrub.NewSecrets(
		"koinon_test_not-a-real-token",
		"short", // below MinSecretLen: ignored
		"  padded-secret-value  \n",
		"line-one-of-a-key\nline-two-of-a-key",
		`has "quotes" and \ slash`,
	)
	s := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: secrets})
	tests := []struct{ name, in, want string }{
		{"verbatim", "x koinon_test_not-a-real-token y", "x " + redacted(scrub.KindSecret) + " y"},
		{"twice", "koinon_test_not-a-real-token|koinon_test_not-a-real-token", redacted(scrub.KindSecret) + "|" + redacted(scrub.KindSecret)},
		{"trimmed", "p padded-secret-value q", "p " + redacted(scrub.KindSecret) + " q"},
		{"short ignored", "a short value", "a short value"},
		{"one line of a multi-line secret", "see line-two-of-a-key.", "see " + redacted(scrub.KindSecret) + "."},
		{"whole multi-line", "line-one-of-a-key\nline-two-of-a-key", redacted(scrub.KindSecret)},
		{"json escaped form", `{"a":"has \"quotes\" and \\ slash"}`, `{"a":"` + redacted(scrub.KindSecret) + `"}`},
		{"prefix only is not a match", "koinon_test_not-a-real-toke", "koinon_test_not-a-real-toke"},
		{"inside a longer word", "AAkoinon_test_not-a-real-tokenBB", "AA" + redacted(scrub.KindSecret) + "BB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Text(tc.in); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
	if scrub.NewSecrets("short").Len() != 0 || (*scrub.Secrets)(nil).Len() != 0 {
		t.Error("short values and nil must be empty sets")
	}
	if strings.Contains(secrets.String()+" "+strings.TrimSpace(secrets.GoString()), "koinon_test") {
		t.Error("Secrets must not print its values")
	}
}

func TestSecretsOverlapAndOrder(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: scrub.NewSecrets("abcdefgh", "defghijk", "efgh-ijkl-mnop")})
	if got, want := s.Text("xx abcdefghijk yy"), "xx "+redacted(scrub.KindSecret)+" yy"; got != want {
		t.Errorf("overlapping secrets: got %q want %q", got, want)
	}
}

func TestOnRedactionCountsByKind(t *testing.T) {
	counts := map[scrub.Kind]int{}
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, OnRedaction: func(k scrub.Kind) { counts[k]++ }})
	s.Text("a@example.com b@example.com " + fakeGatewayKey)
	if counts[scrub.KindEmail] != 2 || counts[scrub.KindGatewayKey] != 1 {
		t.Errorf("counts %v", counts)
	}
}

func TestParseLevel(t *testing.T) {
	for _, l := range []scrub.Level{scrub.LevelNone, scrub.LevelRedacted, scrub.LevelFull} {
		if got, ok := scrub.ParseLevel(l.String()); !ok || got != l {
			t.Errorf("round trip %v", l)
		}
	}
	if _, ok := scrub.ParseLevel("loud"); ok {
		t.Error("unknown level accepted")
	}
}

// TestGolden runs testdata/*.in through each level and compares with <name>.<level>.golden.
// Run with -update to rewrite them after a deliberate change, and read the diff.
func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob("testdata/*.in")
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no golden inputs: %v", err)
	}
	secrets := scrub.NewSecrets("koinon_test_not-a-real-token", "not-a-real-password-1")
	for _, in := range inputs {
		base := strings.TrimSuffix(in, ".in")
		raw, err := os.ReadFile(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, lvl := range []scrub.Level{scrub.LevelFull, scrub.LevelRedacted} {
			t.Run(filepath.Base(base)+"/"+lvl.String(), func(t *testing.T) {
				s := scrub.New(scrub.Config{Level: lvl, Secrets: secrets})
				var got string
				if strings.HasSuffix(base, ".json") {
					out, err := s.JSON(raw)
					if err != nil {
						t.Fatal(err)
					}
					got = string(out) + "\n"
				} else {
					got = s.Text(string(raw))
				}
				golden := base + "." + lvl.String() + ".golden"
				if *update {
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Errorf("%s differs\n--- got\n%s\n--- want\n%s", golden, got, want)
				}
			})
		}
	}
}
