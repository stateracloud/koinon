// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

const plantedSecret = "koinon_test_not-a-real-planted-secret"

// FuzzJSON checks the contract the router relies on: scrubbed JSON is valid JSON, a planted
// secret never survives in a string leaf, and truncation keeps JSON valid and within the limit.
func FuzzJSON(f *testing.F) {
	f.Add(`{"a":"x"}`, 100)
	f.Add(`["`+plantedSecret+`", {"k": "v `+plantedSecret+` w"}]`, 64)
	f.Add(`{"Authorization":"Bearer x","n":[1,2,{"d":null}]}`, 80)
	f.Add(`"`+strings.Repeat("é\"\\\n", 50)+`"`, 70)
	f.Add(`{"n":4111111111111111}`, 1000)
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: scrub.NewSecrets(plantedSecret)})
	f.Fuzz(func(t *testing.T, doc string, limit int) {
		// plant the secret in the document as a string leaf, however the fuzzer shaped the rest
		planted, err := json.Marshal([]any{plantedSecret, "pre " + plantedSecret + " post", json.RawMessage(doc)})
		if err != nil {
			planted, _ = json.Marshal([]any{plantedSecret, "pre " + plantedSecret + " post"})
		}
		out, err := s.JSON(planted)
		if err != nil {
			t.Fatalf("a planted valid document was rejected: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("scrubbed JSON is invalid: %s", out)
		}
		if strings.Contains(string(out), plantedSecret) {
			t.Fatalf("planted secret survived: %s", out)
		}
		limit = scrub.MinMaxBytes + (limit&0x7fffffff)%4096
		cut, _, err := scrub.TruncateJSON(out, limit)
		if err != nil || !json.Valid(cut) || len(cut) > limit {
			t.Fatalf("truncate: err %v valid %v len %d limit %d\n%s", err, json.Valid(cut), len(cut), limit, cut)
		}
		if strings.Contains(string(cut), plantedSecret) {
			t.Fatalf("planted secret appeared after truncation: %s", cut)
		}
		// Content on the raw (possibly invalid) text: never panics, never leaks, respects the limit.
		text, _ := s.Content(strings.Repeat("a", limit%7)+plantedSecret+doc, limit)
		if strings.Contains(text, plantedSecret) || len(text) > limit {
			t.Fatalf("content: len %d limit %d leak %v", len(text), limit, strings.Contains(text, plantedSecret))
		}
	})
}

// FuzzText checks that a planted secret never survives plain-text scrubbing, wherever it sits.
func FuzzText(f *testing.F) {
	f.Add("x", "y")
	f.Add("kgw_", "eyJ")
	f.Add("\x00\xff", "日本")
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: scrub.NewSecrets(plantedSecret)})
	f.Fuzz(func(t *testing.T, before, after string) {
		in := before + plantedSecret + after
		if strings.Contains(s.Text(in), plantedSecret) {
			t.Fatalf("planted secret survived in %q", in)
		}
	})
}
