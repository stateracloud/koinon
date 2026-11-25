// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// The tests of issues 1337 (follow-ups of PR 1325) and 1103. Every key here is made up and
// repeats characters: it is not a real credential.

// TestGitHubShapeAfterPunctuation (issue 1103): a GitHub token shape is redacted behind any one
// or two punctuation characters, such as a quote and a bracket. The exact input of the issue is
// the first body.
func TestGitHubShapeAfterPunctuation(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	bodies := map[string]string{
		"issue 1103 input":       "ghp_o42pond7o1cknullzy9so42pond7o1cknull",
		"ends in a literal":      "ghp_" + strings.Repeat("ab", 17) + "false",
		"holds the alphabet run": "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"oauth token":            "gho_" + strings.Repeat("qz", 18),
		"user to server token":   "ghu_" + strings.Repeat("qz", 18),
		"server to server token": "ghs_" + strings.Repeat("qz", 18),
		"refresh token":          "ghr_" + strings.Repeat("qz", 18),
		"fine-grained token":     "github_pat_" + strings.Repeat("A1", 41),
		"all lower-case letters": "ghp_" + strings.Repeat("abcdefghij", 3) + "abcdef",
		"repeating body":         "ghp_" + strings.Repeat("pond", 9),
	}
	const punct = "\"'[({<,;:=/`>)]}"
	var prefixes []string
	prefixes = append(prefixes, "")
	for i := 0; i < len(punct); i++ {
		prefixes = append(prefixes, punct[i:i+1])
		for j := 0; j < len(punct); j++ {
			prefixes = append(prefixes, punct[i:i+1]+punct[j:j+1])
		}
	}
	for name, key := range bodies {
		for _, p := range prefixes {
			for _, suffix := range []string{"", "\"", " tail"} {
				in := p + key + suffix
				if out := s.Text(in); strings.Contains(out, key) {
					t.Errorf("%s: survived in Text(%q): %q", name, in, out)
				}
				if out, _ := s.Content(in, 0); strings.Contains(out, key) {
					t.Errorf("%s: survived in Content(%q): %q", name, in, out)
				}
			}
		}
	}
}

// TestStopwordNeedsTheWholeCandidate (issue 1337 A): the global stopword is the alphabet run. A
// fixed-shape key that holds the run is still a key. The stopword itself is not a key.
func TestStopwordNeedsTheWholeCandidate(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	const run = "abcdefghijklmnopqrstuvwxyz"
	keys := map[string]string{
		"github pat, run then digits": "ghp_" + run + "0123456789",
		"github pat, digits then run": "ghp_0123456789" + run,
		"github pat, upper-case run":  "ghp_" + strings.ToUpper(run) + "0123456789",
		"github oauth":                "gho_" + run + "0123456789",
		"npm":                         "npm_" + run + "0123456789",
		"gitlab pat":                  "glpat-" + run[:20],
	}
	for name, key := range keys {
		for _, in := range []string{key, "id " + key + " ok", `{"k":"` + key + `"}`, "x=" + key} {
			if out := s.Text(in); strings.Contains(out, key) {
				t.Errorf("%s: survived in Text(%q): %q", name, in, out)
			}
			if out, _ := s.Content(in, 0); strings.Contains(out, key) {
				t.Errorf("%s: survived in Content(%q): %q", name, in, out)
			}
		}
	}
}

// TestAuthHeaderRuleKeepsJSONValid (issue 1337 B): a JSON literal after a credential-like key is
// not a credential. The rule left the literal out of the span but took the comma with it, so the
// scrubbed text was not valid JSON.
func TestAuthHeaderRuleKeepsJSONValid(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for _, in := range []string{
		`{"apiKey": null, "enabled": false}`,
		`{"apiKey":null,"enabled":false}`,
		`{"apiKey": true, "enabled": false}`,
		`{"token": false, "n": 1}`,
		`{"authorization": null, "a": []}`,
		`{"Authorization": null, "x-api-key": null, "n": 1}`,
		`[{"apiKey": null, "enabled": false}, {"n": 2}]`,
		`{"outer": {"apiKey": null, "enabled": false}}`,
		`{"apiKey": null}`,
		`{"enabled": false, "apiKey": null, "n": 3}`,
		`{"apiKey": null }`,
		`{"apiKey" : null , "n": 1}`,
	} {
		if !json.Valid([]byte(in)) {
			t.Fatalf("bad test input: %q", in)
		}
		out := s.Text(in)
		if !json.Valid([]byte(out)) {
			t.Errorf("Text(%q) is not valid JSON: %q", in, out)
		}
		if out != in {
			t.Errorf("Text(%q) changed a value that holds no credential: %q", in, out)
		}
	}
	// A real value next to a literal is still redacted, and the document stays valid.
	in := `{"Authorization": "Bearer notreal-token-value-1234", "apiKey": null, "enabled": false}`
	out := s.Text(in)
	if strings.Contains(out, "notreal-token-value-1234") {
		t.Errorf("credential survived: %q", out)
	}
	if !json.Valid([]byte(out)) || !strings.Contains(out, `"enabled": false`) {
		t.Errorf("document broken: %q", out)
	}
}

// TestBoundSeed (issue 1337 C): the fuzz target cuts its seed. Without the cut the fuzzer grew
// the planted secret to some 100 KB and one execution took seconds.
func TestBoundSeed(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":           {"", ""},
		"short":           {"corpus-word-1", "corpus-word-1"},
		"at the limit":    {strings.Repeat("a", fuzzSeedLimit), strings.Repeat("a", fuzzSeedLimit)},
		"one over":        {strings.Repeat("a", fuzzSeedLimit+1), strings.Repeat("a", fuzzSeedLimit)},
		"a fuzzed growth": {strings.Repeat("w", 1<<20), strings.Repeat("w", fuzzSeedLimit)},
	} {
		if got := boundSeed(tc.in); got != tc.want {
			t.Errorf("%s: got %d bytes, want %d", name, len(got), len(tc.want))
		}
	}
}

// TestLongInputIsScrubbedWhole (issue 1337 C): the cause of the stall is cost, not a missing
// bound in the scrubber, and the fix must not make the scrubber skip long text. A key behind
// 64 KiB of text is still redacted, in every wrapper, and the cost stays in the seconds.
func TestLongInputIsScrubbedWhole(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	key := "ghp_" + strings.Repeat("qz", 18)
	for name, filler := range map[string]string{"letters": "w", "quote and slash": `"\`, "words": "key: "} {
		pad := strings.Repeat(filler, (1<<16)/len(filler))
		in := pad + " " + key + " " + pad
		if out := s.Text(in); strings.Contains(out, key) {
			t.Errorf("%s: survived in Text", name)
		}
		if out, _ := s.Content(in, 0); strings.Contains(out, key) {
			t.Errorf("%s: survived in Content", name)
		}
	}
}

// TestAuthHeaderRuleKeepsStructureForOtherValues: a bare number after a credential-like key is
// redacted, and the delimiter and the rest of the document stay (review of PR 1338: the span
// ended at the next quote, whatever the value was). The marker is not a JSON token, so Text
// output is not valid JSON here; Content, the JSON-aware path, is.
func TestAuthHeaderRuleKeepsStructureForOtherValues(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for _, tc := range []struct{ in, tail string }{
		{`{"apiKey": 0, "n": 1}`, `, "n": 1}`},
		{`{"apiKey": 12, "n": 1}`, `, "n": 1}`},
		{`{"apiKey" : 12 , "n": 1}`, ` , "n": 1}`},
		{`{"apiKey": 12}`, `}`},
		{`[{"token": 7}, {"n": 1}]`, `}, {"n": 1}]`},
	} {
		if out := s.Text(tc.in); !strings.HasSuffix(out, tc.tail) || strings.Contains(out, "12") {
			t.Errorf("Text(%q) = %q, want the tail %q kept and the number redacted", tc.in, out, tc.tail)
		}
		if out, _ := s.Content(tc.in, 0); !json.Valid([]byte(out)) {
			t.Errorf("Content(%q) = %q, want valid JSON", tc.in, out)
		}
	}
}

// TestLiteralThenCredentialStillRedacted: a literal at the start of a value does not hide what
// follows it (review of PR 1338). The value is not a JSON literal here, it is a header line.
func TestLiteralThenCredentialStillRedacted(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	const secret = "Zq8vNotRealOpaqueValue7731"
	for _, in := range []string{
		"Cookie: null; session=" + secret,
		"x-api-key: null, " + secret,
		"X-Api-Key: false; " + secret,
		"authorization: null, " + secret,
		"api_key: null, " + secret,
		"Proxy-Authorization: nil, Basic " + secret,
		`{"authorization": "null, ` + secret + `"}`,
	} {
		if out := s.Text(in); strings.Contains(out, secret) {
			t.Errorf("survived in Text(%q): %q", in, out)
		}
		if out, _ := s.Content(in, 0); strings.Contains(out, secret) {
			t.Errorf("survived in Content(%q): %q", in, out)
		}
	}
}
