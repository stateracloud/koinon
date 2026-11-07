// SPDX-License-Identifier: Apache-2.0

package modelformat_test

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/modelformat"
)

func TestDefault(t *testing.T) {
	cases := []struct {
		provider, model string
		want            modelformat.Format
	}{
		{"openai", "gpt-5", modelformat.OpenAI},
		{"openai", "claude-sonnet-4", modelformat.OpenAI}, // not offered: the provider's first format
		{"ollama", "qwen3:8b", modelformat.OpenAI},
		{"anthropic", "claude-haiku-4-5", modelformat.Anthropic},
		{"anthropic", "anything", modelformat.Anthropic}, // the only format offered
		{"openrouter", "deepseek/deepseek-v4", modelformat.OpenAI},
		{"openrouter", "openai/gpt-5", modelformat.OpenAI},
		{"openrouter", "anthropic/claude-sonnet-4", modelformat.Anthropic}, // the live Messages check passed (old issue 242)
		{"openrouter", "claude-sonnet-4", modelformat.Anthropic},
		{"ollama", "claude-sonnet-4", modelformat.OpenAI},
		{"custom-vendor", "Claude-Opus", modelformat.OpenAI},
	}
	for _, c := range cases {
		if got := modelformat.Default(c.provider, c.model); got != c.want {
			t.Errorf("Default(%s, %s) = %s, want %s", c.provider, c.model, got, c.want)
		}
	}
}

func TestPreferredIgnoresWhatIsOffered(t *testing.T) {
	if got := modelformat.Preferred("openai", "claude-sonnet-4"); got != modelformat.Anthropic {
		t.Errorf("Preferred = %s, want anthropic", got)
	}
	if got := modelformat.Preferred("openrouter", "anthropic/claude-sonnet-4"); got != modelformat.Anthropic {
		t.Errorf("Preferred = %s, want anthropic (the OpenRouter pin is gone, old issue 242)", got)
	}
	if got := modelformat.Preferred("x", "anthropic/claude-sonnet-4"); got != modelformat.Anthropic {
		t.Errorf("Preferred = %s, want anthropic", got)
	}
}

func TestValidate(t *testing.T) {
	ok := [][2]string{{"anthropic", "anthropic"}, {"openrouter", "openai"}, {"openrouter", "anthropic"}, {"openai", "openai"}}
	for _, c := range ok {
		if err := modelformat.Validate(c[0], modelformat.Format(c[1])); err != nil {
			t.Errorf("Validate(%s, %s): %v", c[0], c[1], err)
		}
	}
	bad := map[[2]string]string{
		{"openai", "anthropic"}:    "does not offer",
		{"anthropic", "openai"}:    "does not offer",
		{"openrouter", "gemini"}:   "unknown model format",
		{"openrouter", ""}:         "unknown model format",
		{"ollama", "anthropic"}:    "offers openai",
		{"anthropic", "Anthropic"}: "unknown model format",
	}
	for c, want := range bad {
		err := modelformat.Validate(c[0], modelformat.Format(c[1]))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s, %q) = %v, want an error containing %q", c[0], c[1], err, want)
		}
	}
}

func TestResolve(t *testing.T) {
	f, over, err := modelformat.Resolve("openrouter", "anthropic/claude-sonnet-4", "")
	if err != nil || over || f != modelformat.Anthropic {
		t.Errorf("default: %s %v %v", f, over, err)
	}
	f, over, err = modelformat.Resolve("openrouter", "anthropic/claude-sonnet-4", "openai")
	if err != nil || !over || f != modelformat.OpenAI {
		t.Errorf("override: %s %v %v", f, over, err)
	}
	if _, _, err = modelformat.Resolve("openai", "gpt-5", "anthropic"); err == nil {
		t.Error("an override the provider does not offer was accepted")
	}
}

// Every provider type's first offered format and every preference must be a known format, and
// every format must be offered by someone, so a new format cannot be added half way.
func TestTableIsConsistent(t *testing.T) {
	for _, f := range modelformat.All() {
		if !modelformat.Known(f) || modelformat.Label(f) == string(f) {
			t.Errorf("format %s has no label", f)
		}
	}
	for _, p := range []string{"anthropic", "openrouter", "openai", "unlisted"} {
		offered := modelformat.Offered(p)
		if len(offered) == 0 {
			t.Fatalf("%s offers nothing", p)
		}
		for _, f := range offered {
			if !modelformat.Known(f) {
				t.Errorf("%s offers the unknown format %s", p, f)
			}
		}
		// The slice is a copy: changing it must not change the table.
		offered[0] = "mutated"
		if modelformat.Offered(p)[0] == "mutated" {
			t.Errorf("Offered(%s) leaks its table", p)
		}
	}
}

func TestWorkspaceIDMatchesMigration(t *testing.T) {
	sql, err := os.ReadFile("../db/migrations/00031_model_formats.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := "CHECK (anthropic_workspace_id ~ '" + modelformat.WorkspaceIDPattern + "' AND length(anthropic_workspace_id) <= " + strconv.Itoa(modelformat.MaxWorkspaceIDLength) + ")"
	if !strings.Contains(string(sql), want) {
		t.Errorf("migration 00031 does not contain %q", want)
	}
	for id, ok := range map[string]bool{
		"wrkspc_01Abc": true, "wrkspc_": false, "ws_01": false, "wrkspc_01\r\nx: y": false, "wrkspc_01 ": false,
		"wrkspc_" + strings.Repeat("a", modelformat.MaxWorkspaceIDLength-7): true,
		"wrkspc_" + strings.Repeat("a", modelformat.MaxWorkspaceIDLength-6): false,
	} {
		if got := modelformat.ValidWorkspaceID(id); got != ok {
			t.Errorf("ValidWorkspaceID(%q) = %v, want %v", id, got, ok)
		}
	}
}
