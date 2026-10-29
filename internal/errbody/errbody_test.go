// SPDX-License-Identifier: Apache-2.0

package errbody_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/errbody"
)

func TestExcerptCollapsesWhitespaceAndRedacts(t *testing.T) {
	got := errbody.Excerpt("  session/prompt: Authentication \n required\t(Bearer abcdef1234567890) ")
	if got != "session/prompt: Authentication required ("+errbody.Redacted+")" {
		t.Errorf("got %q", got)
	}
	if errbody.Excerpt("   ") != "" {
		t.Error("an empty text gives an empty excerpt")
	}
}

func TestRedactRemovesCredentials(t *testing.T) {
	for name, in := range map[string]string{
		"bearer":         "Authorization failed for Bearer abcdef1234567890",
		"openrouter key": "key sk-or-v1-0123456789abcdef0123456789abcdef is invalid",
		"openai key":     "Incorrect API key provided: sk-proj-abcdefghijkl",
		"koinon key":     "got kgw_AbCdEfGhIjKlMnOp",
		"json field":     `{"api_key": "supersecretvalue", "x": 1}`,
		"long token":     "token 0123456789abcdef0123456789abcdef01234567 rejected",
	} {
		t.Run(name, func(t *testing.T) {
			got := errbody.Redact(in)
			for _, secret := range []string{"abcdef1234567890", "0123456789abcdef0123456789abcdef", "abcdefghijkl", "AbCdEfGhIjKlMnOp", "supersecretvalue"} {
				if strings.Contains(got, secret) {
					t.Errorf("%q still carries %q", got, secret)
				}
			}
			if !strings.Contains(got, "redacted") {
				t.Errorf("nothing was redacted in %q", got)
			}
		})
	}
	if got := errbody.Redact("model gen-1793280091-oTLlQhsYNSde4WT2kATg failed"); strings.Contains(got, "redacted") {
		t.Errorf("a response ID is not a credential: %q", got)
	}
}

func TestExcerptCapsAndKeepsValidUTF8(t *testing.T) {
	got := errbody.Excerpt(strings.Repeat("é", 5000))
	if n := utf8.RuneCountInString(got); n != errbody.ExcerptMax {
		t.Errorf("excerpt has %d characters, want %d", n, errbody.ExcerptMax)
	}
	if !utf8.ValidString(got) {
		t.Error("excerpt is not valid UTF-8")
	}
}
