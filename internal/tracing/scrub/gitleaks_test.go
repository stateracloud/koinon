// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/scrub/gitleaksrules"
)

func TestGitleaksRulesCompile(t *testing.T) {
	set, err := gitleaksrules.Load()
	if err != nil {
		t.Fatal(err)
	}
	if set.Version == "" || len(set.Rules) < 100 {
		t.Fatalf("rule set looks empty: version %q, %d rules", set.Version, len(set.Rules))
	}
	if got := scrub.GitleaksSkipped(); len(got) > 0 {
		t.Fatalf("rules that did not compile: %v", got)
	}
	// The rules are the release CI scans with: a bump of GITLEAKS_VERSION regenerates them.
	env, err := os.ReadFile("../../../hack/versions.env")
	if err != nil {
		t.Fatal(err)
	}
	if want := "GITLEAKS_VERSION=" + set.Version + "\n"; !strings.Contains(string(env), want) {
		t.Fatalf("hack/versions.env does not pin %q: run hack/gitleaks-rules.sh after a bump", want)
	}
}

// Fake credentials for the tests, built at run time from a fixed alphabet of distinct characters
// (about five bits each), so they pass the entropy floor of the rules that keep one and the source
// holds no key-shaped literal for gitleaks to find in this repository.
const (
	fakeAlnum = "Kq7ZvLm2XpR9wTy4NbCj8dFhGs3AeUx5" // 32 distinct characters
	fakeHex   = "3fa9c07e5d1b8246"                 // 16 distinct hex digits
	fakeAlpha = "qzkvlmxprwtynbcjdfhgsaeuoi"       // 26 distinct lower-case letters
	fakeUpper = "KQZVLMXPRWTYNBCJDFHGSAEU234567"   // distinct, within A-Z2-7
)

// fake returns n characters cycling through alphabet from offset off.
func fake(alphabet string, n, off int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[(i+off)%len(alphabet)]
	}
	return string(b)
}

func fk(n int) string { return fake(fakeAlnum, n, 0) }

func TestGitleaksKnownShapes(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	redacted := regexp.MustCompile(`\[redacted:[a-z-]+\]`)
	for name, in := range map[string]string{
		"github pat":    "token ghp_" + fk(36),
		"aws key id":    "id AKIA" + fake(fakeUpper, 16, 0) + " ok",
		"stripe":        "sk_live_" + fk(24),
		"slack bot":     "xoxb-" + fake("1234567890", 10, 0) + "-" + fake("1234567890", 13, 0) + "-" + fk(24),
		"gitlab pat":    "glpat-" + fk(20),
		"huggingface":   "hf_" + fake(fakeAlpha, 34, 0),
		"digitalocean":  "dop_v1_" + fake(fakeHex, 64, 0),
		"in json value": `{"env":{"K":"ghp_` + fk(36) + `"}}`,
	} {
		out := s.Text(in)
		if !redacted.MatchString(out) {
			t.Errorf("%s: not redacted: %q", name, out)
		}
	}
}

// The shapes the scrubber adds because gitleaks has no rule for them: when a gitleaks bump adds
// one, this fails and the shape leaves gapKeyRE (see gitleaks.go).
func TestGapShapesAreNotInGitleaks(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for name, in := range map[string]string{
		"openrouter":      "x sk-or-v1-" + fake(fakeHex, 64, 0),
		"anthropic short": "x sk-ant-api03-" + fk(20),
		"google oauth":    "x ya29." + fk(40),
		"google client":   "x GOCSPX-" + fk(28),
	} {
		if ids, _ := scrub.GitleaksMatches(in); len(ids) > 0 {
			t.Errorf("%s: gitleaks now covers it (%v): remove it from gapKeyRE", name, ids)
		}
		if out := s.Text(in); !strings.Contains(out, "[redacted:api-key]") || out[:2] != "x " {
			t.Errorf("%s: not redacted: %q", name, out)
		}
	}
}

func TestGitleaksExemptions(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for name, in := range map[string]string{
		"git sha":    "commit 3f2b8c1e9a4d4e7b8c215d6f7a8b9c0d1e2f3a4b fixes it",
		"trace id":   `{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}`,
		"digest":     "registry.example/image@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"prose":      "The key to a good review is to read the diff and the token budget before the tests.",
		"model name": "claude-sonnet-4-5-20250929 via openrouter",
	} {
		if out := s.Text(in); out != in {
			t.Errorf("%s: changed: %q", name, out)
		}
	}
}
