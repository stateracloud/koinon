// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"encoding/base64"
	"slices"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

const fakeMCPToken = "koinon_test_not-a-real-token"

// Redact removes known values and their encoded forms, and nothing else: no shape rule runs (#1174).
func TestSecretsRedactExactValuesOnly(t *testing.T) {
	set := scrub.NewSecrets(fakeMCPToken)
	sec := scrub.Replacement(scrub.KindSecret)
	for in, want := range map[string]string{
		"t=" + fakeMCPToken: "t=" + sec,
		"b=" + base64.StdEncoding.EncodeToString([]byte(fakeMCPToken)): "b=" + sec,
		"Bearer " + fakeMCPToken:                    "Bearer " + sec,
		"Bearer koinon_test_other-not-a-real-token": "Bearer koinon_test_other-not-a-real-token", // a shape, not a known value
		"kgw_koinon_test_not-a-real-key":            "kgw_koinon_test_not-a-real-key",
		"":                                          "",
	} {
		if got := set.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	var none *scrub.Secrets
	if got := none.Redact(fakeMCPToken); got != fakeMCPToken {
		t.Errorf("a nil set changed the text: %q", got)
	}
}

func TestAuthParts(t *testing.T) {
	if got := scrub.AuthParts("Bearer " + fakeMCPToken); !slices.Equal(got, []string{"Bearer " + fakeMCPToken, fakeMCPToken}) {
		t.Errorf("Bearer: %q", got)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("koinon_test_user:" + fakeMCPToken))
	got := scrub.AuthParts("Basic " + basic)
	for _, want := range []string{basic, "koinon_test_user:" + fakeMCPToken, fakeMCPToken} {
		if !slices.Contains(got, want) {
			t.Errorf("Basic: %q lacks %q", got, want)
		}
	}
	if slices.Contains(got, "koinon_test_user") {
		t.Errorf("Basic: %q holds the user name, which is not a secret", got)
	}
	for _, pass := range []string{"", "x"} {
		keyAsUser := base64.StdEncoding.EncodeToString([]byte(fakeMCPToken + ":" + pass))
		if got := scrub.AuthParts("Basic " + keyAsUser); !slices.Contains(got, fakeMCPToken) {
			t.Errorf("Basic with password %q: %q lacks the user name, which carries the secret", pass, got)
		}
	}
	if got := scrub.AuthParts(fakeMCPToken); !slices.Equal(got, []string{fakeMCPToken}) {
		t.Errorf("no scheme: %q", got)
	}
}

// Encoders other than Go's write lower-case hex digits in a percent escape, and some JSON encoders write / as
// \/. The value keeps its case in both forms, so the whole-lower form does not match them (#1174).
func TestSecretsRedactOtherEncoderForms(t *testing.T) {
	const v = "KOINON/test+not real=token"
	set := scrub.NewSecrets(v)
	sec := scrub.Replacement(scrub.KindSecret)
	for _, in := range []string{
		"KOINON%2ftest%2bnot+real%3dtoken", // query escape, lower-case hex
		"KOINON%2Ftest%2Bnot+real%3Dtoken", // query escape, Go's upper-case hex
		"KOINON%2ftest+not%20real=token",   // path escape, lower-case hex
		`KOINON\/test+not real=token`,      // JSON with an escaped slash
		"koinon%2ftest%2bnot+real%3dtoken", // a server that folds the URL to lower case
	} {
		if got := set.Redact("q=" + in); got != "q="+sec {
			t.Errorf("Redact(%q) = %q, want the value removed", in, got)
		}
	}
}
