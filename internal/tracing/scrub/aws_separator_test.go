// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// TestAWSKeyIDAfterSeparator is the plain spec for issue 1299. The fuzz seed 0ad82284c64d31d2 of
// FuzzAlwaysShapesNeverSurvive found a key ID that survived behind a colon. The colon was not the
// cause: the upstream global allowlist `(?i)^true|false|null$` has no group, so a secret that
// ends in "null" (or holds "false", or starts with "true") was dropped as a literal. The key IDs
// below are fake and built at run time; the documentation key ID ending in EXAMPLE is allowlisted
// by the AWS rule itself and is not a case here.
func TestAWSKeyIDAfterSeparator(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	tails := map[string]string{
		"plain":          fake(fakeUpper, 16, 3),
		"ends in NULL":   "H37NAM2U7WZQ" + "NULL",
		"holds FALSE":    "H37NAM2U7" + "FALSE" + "Q2",
		"starts in TRUE": "TRUE" + "H37NAM2U7WZQ",
	}
	for tname, tail := range tails {
		for _, prefix := range []string{"AKIA", "ASIA"} {
			id := prefix + tail
			if len(tail) != 16 {
				t.Fatalf("bad test tail %q", tail)
			}
			for cname, in := range map[string]string{
				"bare":         id,
				"key:id":       "key:" + id,
				"key: id":      "key: " + id,
				"colon first":  ":" + id,
				"json":         `{"k":"` + id + `"}`,
				"json spaced":  `{"k": "` + id + `"}`,
				"url query":    "https://host.example.test/p?access_key_id=" + id + "&x=1",
				"url colon":    "https://host.example.test:8443/p?k:" + id,
				"yaml":         "k: " + id + "\n",
				"escaped json": `"{\"k\":\"` + id + `\"}"`,
			} {
				t.Run(tname+"/"+prefix+"/"+cname, func(t *testing.T) {
					out := s.Text(in)
					if strings.Contains(out, tail) {
						t.Fatalf("key ID survived Text\n in  %q\n out %q", in, out)
					}
					out, _ = s.Content(in, 0)
					if strings.Contains(out, tail) {
						t.Fatalf("key ID survived Content\n in  %q\n out %q", in, out)
					}
				})
			}
		}
	}
}

// TestLiteralsStayReadable keeps what the fixed global allowlist is for: a value that is only
// true, false or null is not a secret and is not redacted, and the AWS documentation key ID stays.
func TestLiteralsStayReadable(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for _, in := range []string{
		`{"enabled":false,"x":null,"y":true}`,
		"retry=false next=null ok=TRUE",
		"AKIAIOSFODNN7" + "EXAMPLE",
	} {
		if out := s.Text(in); out != in {
			t.Errorf("over-redaction: %q became %q", in, out)
		}
	}
}
