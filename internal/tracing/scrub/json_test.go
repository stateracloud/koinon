// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

func TestJSON(t *testing.T) {
	secrets := scrub.NewSecrets("koinon_test_not-a-real-token")
	full := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: secrets})
	red := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: secrets})
	tests := []struct {
		name, in   string
		full, redd string // "" means the same as the other level's want, or the compact input
	}{
		{
			"keeps structure and order",
			`{"b": 1, "a": [true, null, 1.50, "x"], "c": {"z": "y"}}`,
			`{"b":1,"a":[true,null,1.50,"x"],"c":{"z":"y"}}`, "",
		},
		{
			"string leaves",
			`["use koinon_test_not-a-real-token", "mail a@example.com"]`,
			`["use [redacted:secret]","mail a@example.com"]`,
			`["use [redacted:secret]","mail [redacted:email]"]`,
		},
		{
			"keys are scrubbed too",
			`{"a@example.com": 1}`, `{"a@example.com":1}`, `{"[redacted:email]":1}`,
		},
		{
			"header keys replace whole values",
			`{"headers": {"Authorization": "Basic Zm9v", "Accept": "text/plain", "cookie": "a=b", "X-Api-Key": "k"}}`,
			`{"headers":{"Authorization":"[redacted:auth-header]","Accept":"text/plain","cookie":"[redacted:cookie]","X-Api-Key":"[redacted:auth-header]"}}`, "",
		},
		{
			"header list",
			`[{"name": "Set-Cookie", "value": "sid=1"}, {"key": "Accept", "value": "x"}]`,
			`[{"name":"Set-Cookie","value":"[redacted:cookie]"},{"key":"Accept","value":"x"}]`, "",
		},
		{
			"header array values",
			`{"Cookie": ["a=b", "c=d"], "n": 1}`, `{"Cookie":["[redacted:cookie]","[redacted:cookie]"],"n":1}`, "",
		},
		{"scalars under a header key are replaced", `{"authorization": true, "cookie": 3, "x-api-key": null}`, `{"authorization":"[redacted:auth-header]","cookie":"[redacted:cookie]","x-api-key":null}`, ""},
		{"card as a number", `{"n": 4111111111111111, "ok": 12345}`, `{"n":4111111111111111,"ok":12345}`, `{"n":"[redacted:card]","ok":12345}`},
		{"escapes survive", `"line\nbreak \"quoted\" <tag> \u2028"`, `"line\nbreak \"quoted\" <tag> \u2028"`, ""},
		{"scalar string", `"x"`, `"x"`, ""},
		{"empty containers", `{"a":{},"b":[]}`, `{"a":{},"b":[]}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantRed := tc.redd
			if wantRed == "" {
				wantRed = tc.full
			}
			for _, c := range []struct {
				s    *scrub.Scrubber
				want string
			}{{full, tc.full}, {red, wantRed}} {
				got, err := c.s.JSON([]byte(tc.in))
				if err != nil {
					t.Fatalf("%s: %v", c.s.Level(), err)
				}
				if string(got) != c.want {
					t.Errorf("%s:\n got %s\nwant %s", c.s.Level(), got, c.want)
				}
				if !json.Valid(got) {
					t.Errorf("%s: invalid JSON %s", c.s.Level(), got)
				}
			}
		})
	}
}

func TestJSONRejectsBadInput(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for _, in := range []string{``, `{`, `{"a":}`, `[1,]`, `1 2`, `{"a":1} x`} {
		if out, err := s.JSON([]byte(in)); !errors.Is(err, scrub.ErrInvalidJSON) || out != nil {
			t.Errorf("%.20q: out %q err %v", in, out, err)
		}
	}
}

func TestTruncateJSONFits(t *testing.T) {
	out, cut, err := scrub.TruncateJSON([]byte(`{ "a" : [1, 2] }`), 0)
	if err != nil || cut || string(out) != `{"a":[1,2]}` {
		t.Errorf("out %s cut %v err %v", out, cut, err)
	}
	if _, _, err := scrub.TruncateJSON([]byte(`{`), 0); !errors.Is(err, scrub.ErrInvalidJSON) {
		t.Errorf("err %v", err)
	}
}

func TestTruncateJSONShapes(t *testing.T) {
	long := strings.Repeat("abcdefghij", 500) // 5000 bytes
	tests := []struct {
		name, in string
		limit    int
		check    func(t *testing.T, out string)
	}{
		{"long string", `"` + long + `"`, 200, func(t *testing.T, out string) {
			if !strings.HasSuffix(out, scrub.TruncatedSuffix+`"`) || !strings.HasPrefix(out, `"abcdefghij`) {
				t.Errorf("%s", out)
			}
		}},
		{"many elements", "[" + strings.TrimSuffix(strings.Repeat(`"element",`, 200), ",") + "]", 300, func(t *testing.T, out string) {
			var v []string
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(v[len(v)-1], "more items]") || v[0] != "element" {
				t.Errorf("%s", out)
			}
		}},
		{"many members", func() string {
			var parts []string
			for i := 0; i < 100; i++ {
				parts = append(parts, fmt.Sprintf(`"key%03d":"value%03d"`, i, i))
			}
			return "{" + strings.Join(parts, ",") + "}"
		}(), 400, func(t *testing.T, out string) {
			var v map[string]any
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Fatal(err)
			}
			if n, ok := v[scrub.TruncatedKey].(float64); !ok || n < 1 || v["key000"] != "value000" {
				t.Errorf("%s", out)
			}
		}},
		{
			"a huge system prompt does not push out the user message",
			`[{"role":"system","content":"` + strings.Repeat("rules ", 5000) + `"},{"role":"user","content":"What is the capital of France?"}]`,
			1024, func(t *testing.T, out string) {
				if !strings.Contains(out, "What is the capital of France?") || !strings.Contains(out, scrub.TruncatedSuffix) {
					t.Errorf("%s", out)
				}
			},
		},
		{"nested", `{"messages":[{"role":"tool","parts":[{"text":"` + long + `"},{"text":"` + long + `"}]}],"n":1}`, 512, func(t *testing.T, out string) {
			if !strings.Contains(out, `"n":1`) {
				t.Errorf("%s", out)
			}
		}},
		{"multibyte and escapes", func() string { b, _ := json.Marshal(strings.Repeat("é\n\"日本", 400)); return string(b) }(), 100, func(t *testing.T, out string) {
			var s string
			if err := json.Unmarshal([]byte(out), &s); err != nil || !utf8.ValidString(s) {
				t.Errorf("%v %q", err, out)
			}
		}},
		{"tiny limit is raised", `"` + long + `"`, 1, func(t *testing.T, out string) {
			if len(out) > scrub.MinMaxBytes || !json.Valid([]byte(out)) {
				t.Errorf("%s", out)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, cut, err := scrub.TruncateJSON([]byte(tc.in), tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			limit := max(tc.limit, scrub.MinMaxBytes)
			if !cut || len(out) > limit || !json.Valid(out) {
				t.Fatalf("cut=%v len=%d limit=%d valid=%v\n%s", cut, len(out), limit, json.Valid(out), out)
			}
			tc.check(t, string(out))
		})
	}
}

func TestTruncateJSONDefaultLimit(t *testing.T) {
	in, _ := json.Marshal([]string{strings.Repeat("x", 40<<10)})
	out, cut, err := scrub.TruncateJSON(in, 0)
	if err != nil || !cut || len(out) > scrub.DefaultMaxBytes || !json.Valid(out) {
		t.Errorf("len %d cut %v err %v", len(out), cut, err)
	}
}

func TestTruncateText(t *testing.T) {
	s := strings.Repeat("日本語", 100)
	out, cut := scrub.TruncateText(s, 100)
	if !cut || len(out) > 100 || !utf8.ValidString(out) || !strings.HasSuffix(out, scrub.TruncatedSuffix) {
		t.Errorf("%q", out)
	}
	if out, cut := scrub.TruncateText("short", 100); cut || out != "short" {
		t.Errorf("%q %v", out, cut)
	}
}

func TestContent(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: scrub.NewSecrets("koinon_test_not-a-real-token")})
	t.Run("json", func(t *testing.T) {
		in := `[{"role":"user","content":"hi jane@example.com ` + strings.Repeat("x", 4000) + `"}]`
		out, cut := s.Content(in, 512)
		if !cut || len(out) > 512 || !json.Valid([]byte(out)) || strings.Contains(out, "jane@") || !strings.Contains(out, "[redacted:email]") {
			t.Errorf("%s", out)
		}
	})
	t.Run("text", func(t *testing.T) {
		out, cut := s.Content("note koinon_test_not-a-real-token "+strings.Repeat("y", 400), 100)
		if !cut || len(out) > 100 || strings.Contains(out, "koinon_test") || !strings.HasPrefix(out, "note [redacted:secret]") {
			t.Errorf("%q", out)
		}
	})
	t.Run("secret is replaced before the cut", func(t *testing.T) {
		// the cut would land inside the secret; scrub-first means no half secret survives
		pad := strings.Repeat("p", 60)
		out, _ := s.Content(pad+"koinon_test_not-a-real-token", 80)
		if strings.Contains(out, "koinon_test") {
			t.Errorf("%q", out)
		}
	})
	t.Run("json-looking text that is not JSON is text", func(t *testing.T) {
		out, cut := s.Content("[not json mail a@example.com", 0)
		if cut || out != "[not json mail [redacted:email]" {
			t.Errorf("%q", out)
		}
	})
	t.Run("a scalar JSON string", func(t *testing.T) {
		out, _ := s.Content(`"mail a@example.com"`, 0)
		if out != `"mail [redacted:email]"` {
			t.Errorf("%q", out)
		}
	})
}

func TestJSONOutputIsCompactAndStable(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted})
	in := []byte("{\n  \"a\": [\n    1,\n    2\n  ]\n}\n")
	a, _ := s.JSON(in)
	b, _ := s.JSON(a)
	if !bytes.Equal(a, b) || bytes.ContainsAny(a, " \n") {
		t.Errorf("%q %q", a, b)
	}
}

func TestTooDeepJSONIsAPlaceholderNeverText(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	deep := strings.Repeat("[", 300) + `"` + esc(`kgw~u005fnotrealnotrealnotreal`) + `"` + strings.Repeat("]", 300)
	out, err := s.JSON([]byte(deep))
	if err != nil || string(out) != scrub.TooDeepPlaceholder {
		t.Errorf("JSON: %q %v", out, err)
	}
	got, cut := s.Content(deep, 0)
	if got != scrub.TooDeepPlaceholder || !cut {
		t.Errorf("Content: %q %v", got, cut)
	}
	tr, cut, err := scrub.TruncateJSON([]byte(deep), 0)
	if err != nil || string(tr) != scrub.TooDeepPlaceholder || !cut {
		t.Errorf("TruncateJSON: %q %v %v", tr, cut, err)
	}
}
