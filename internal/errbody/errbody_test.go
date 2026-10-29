// SPDX-License-Identifier: Apache-2.0

package errbody_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/errbody"
)

func TestSummarizeReadsTheKnownShapes(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"openrouter moderation": {
			`{"error":{"code":403,"message":"Input flagged by moderation","metadata":{"reasons":["violence","hate"],"flagged_input":"the user's prompt text","provider_name":"OpenAI"}}}`,
			"403: Input flagged by moderation (reasons: violence, hate; provider: OpenAI)",
		},
		"openai": {
			`{"error":{"message":"Invalid value for 'messages'.","type":"invalid_request_error","code":"invalid_value"}}`,
			"invalid_request_error/invalid_value: Invalid value for 'messages'.",
		},
		"anthropic": {
			`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"},"request_id":"req_1"}`,
			"invalid_request_error: max_tokens: must be greater than 0",
		},
		"string error": {`{"error":"bad thing"}`, "bad thing"},
		"message only": {`{"message":"nope"}`, "nope"},
		"plain text":   {"  Forbidden \n by the\tproxy ", "Forbidden by the proxy"},
		"empty":        {"   ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := errbody.Summarize(tc.body); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummarizeDropsPromptFragmentsFromKnownFields(t *testing.T) {
	got := errbody.Summarize(`{"error":{"code":403,"message":"flagged","metadata":{"flagged_input":"SECRET PROMPT TEXT","raw":"MORE PROMPT"}}}`)
	if strings.Contains(got, "PROMPT") {
		t.Errorf("the excerpt carries prompt text: %q", got)
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

func TestSummarizeCapsAndKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("é", 5000)
	got := errbody.Summarize(long)
	if n := utf8.RuneCountInString(got); n != errbody.ExcerptMax {
		t.Errorf("excerpt has %d characters, want %d", n, errbody.ExcerptMax)
	}
	if !utf8.ValidString(got) {
		t.Error("excerpt is not valid UTF-8")
	}
	msg := errbody.Summarize(`{"error":{"message":"` + strings.Repeat("a b ", 500) + `"}}`)
	if utf8.RuneCountInString(msg) > errbody.ExcerptMax {
		t.Errorf("excerpt too long: %d", utf8.RuneCountInString(msg))
	}
}

func TestGatewayExprIsOneConditionalOverTheBodyPrefix(t *testing.T) {
	e := errbody.GatewayExpr()
	for _, want := range []string{"response.code >= 400", "response.bodyPrefix", "fail(", "regexReplaceAll("} {
		if !strings.Contains(e, want) {
			t.Errorf("expression lacks %q: %s", want, e)
		}
	}
}

func TestGatewayExprNeverCopiesAJSONBody(t *testing.T) {
	e := errbody.GatewayExpr()
	if strings.Contains(e, "$") {
		t.Errorf("expression contains $, which the gateway's config loader expands: %s", e)
	}
	// The structured form is built field by field, and the body is only copied when it is not JSON.
	for _, want := range []string{"coalesce(", "json(response.bodyPrefix)", "toJson(", "unrecognised shape", "startsWith("} {
		if !strings.Contains(e, want) {
			t.Errorf("expression lacks %q: %s", want, e)
		}
	}
	for _, banned := range []string{"flagged_input", "raw"} {
		if strings.Contains(e, banned) {
			t.Errorf("expression names the prompt-bearing field %q", banned)
		}
	}
}
