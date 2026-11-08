// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/declarative"
)

func providerSecret(provider, workspace, models string) *corev1.Secret {
	data := map[string][]byte{
		"name": []byte("p"), "provider": []byte(provider), "url": []byte("https://api.example.com/v1"),
		"models": []byte(models),
	}
	if workspace != "" {
		data["anthropicWorkspaceId"] = []byte(workspace)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Data: data}
}

// TestParseProviderFormats covers the per-model format and the workspace ID (ADR 0069), validated
// by the same rules as the API.
func TestParseProviderFormats(t *testing.T) {
	d, err := declarative.ParseProvider(providerSecret("openrouter", "", "- name: anthropic/claude-a\n  format: anthropic\n- name: plain\n"), "v")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Models) != 2 || d.Models[0].Format != "anthropic" || d.Models[1].Format != "" {
		t.Errorf("models = %+v", d.Models)
	}
	d, err = declarative.ParseProvider(providerSecret("anthropic", "wrkspc_01Ab", "- name: claude-haiku-4-5\n"), "v")
	if err != nil || d.AnthropicWorkspaceID != "wrkspc_01Ab" {
		t.Errorf("workspace: %q, %v", d.AnthropicWorkspaceID, err)
	}

	bad := []struct {
		name, want string
		sec        *corev1.Secret
	}{
		{"a format the provider does not offer", "does not offer", providerSecret("openai", "", "- name: gpt\n  format: anthropic\n")},
		{"an unknown format", "unknown model format", providerSecret("openrouter", "", "- name: m\n  format: gemini\n")},
		{"a workspace ID on another type", "anthropic providers only", providerSecret("openai", "wrkspc_01", "")},
		{"a malformed workspace ID", "wrkspc_", providerSecret("anthropic", "ws-1", "")},
	}
	for _, c := range bad {
		if _, err := declarative.ParseProvider(c.sec, "v"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

// TestParseProviderCostKey covers the optional cost API key and scope, validated by the rules of the
// API, with errors that never quote a value.
func TestParseProviderCostKey(t *testing.T) {
	const key = "koinon_test_not-a-real-admin-key"
	with := func(provider, url, costKey, scope string) *corev1.Secret {
		sec := providerSecret(provider, "", "")
		sec.Data["url"] = []byte(url)
		if costKey != "" {
			sec.Data["costApiKey"] = []byte(costKey + "\n")
		}
		if scope != "" {
			sec.Data["costScope"] = []byte(scope)
		}
		return sec
	}
	d, err := declarative.ParseProvider(with("anthropic", "https://api.anthropic.com", key, "wrkspc_01Ab"), "v")
	if err != nil || d.CostAPIKey != key || d.CostScope != "wrkspc_01Ab" {
		t.Errorf("anthropic: %+v, %v", d, err)
	}
	d, err = declarative.ParseProvider(with("openai", "https://api.openai.com/v1", key, "proj_abc"), "v")
	if err != nil || d.CostAPIKey != key || d.CostScope != "proj_abc" {
		t.Errorf("openai: %+v, %v", d, err)
	}
	d, err = declarative.ParseProvider(with("anthropic", "https://api.anthropic.com", "", ""), "v")
	if err != nil || d.CostAPIKey != "" || d.CostScope != "" {
		t.Errorf("none: %+v, %v", d, err)
	}
	for name, sec := range map[string]*corev1.Secret{
		"key without a scope":        with("anthropic", "https://api.anthropic.com", key, ""),
		"scope without a key":        with("anthropic", "https://api.anthropic.com", "", "wrkspc_01Ab"),
		"a scope of the wrong kind":  with("anthropic", "https://api.anthropic.com", key, "proj_abc"),
		"a type without a cost API":  with("openrouter", "https://openrouter.ai/api/v1", key, "proj_abc"),
		"not the provider's own API": with("anthropic", "https://proxy.example.com", key, "wrkspc_01Ab"),
	} {
		_, err := declarative.ParseProvider(sec, "v")
		if err == nil {
			t.Errorf("%s: parsed", name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
}
