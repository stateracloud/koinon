// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/declarative"
)

// secretManifest is the part of a Secret manifest the importer reads, with stringData.
type secretManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	StringData map[string]string `yaml:"stringData"`
}

// parseManifest turns a manifest as a reader would apply it into the Secret the importer sees.
func parseManifest(t *testing.T, where, doc, want string) *corev1.Secret {
	t.Helper()
	var m secretManifest
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	if m.APIVersion != "v1" || m.Kind != "Secret" {
		t.Fatalf("%s: want a v1 Secret, got %s %s", where, m.APIVersion, m.Kind)
	}
	if m.Metadata.Labels[declarative.LabelSecretType] != want || m.Metadata.Labels[declarative.LabelOrg] == "" {
		t.Fatalf("%s: the Secret needs the labels %s=%s and %s", where, declarative.LabelSecretType, want, declarative.LabelOrg)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: m.Metadata.Name, Namespace: m.Metadata.Namespace, Labels: m.Metadata.Labels}, Data: map[string][]byte{}}
	for k, v := range m.StringData {
		sec.Data[k] = []byte(v)
	}
	return sec
}

// TestDocumentedSecretsParse keeps every model-provider manifest the docs, the e2e test and the dev
// profiles show valid for the importer, so a change to the Secret's format cannot leave one behind.
func TestDocumentedSecretsParse(t *testing.T) {
	root := filepath.Join("..", "..")
	block := regexp.MustCompile("(?s)```yaml model-provider\n(.*?)```")
	n := 0
	for _, f := range []string{"docs/site/src/content/docs/quickstart.md", "docs/site/src/content/docs/guides/models.md"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range block.FindAllStringSubmatch(string(raw), -1) {
			n++
			sec := parseManifest(t, f, m[1], declarative.TypeModelProvider)
			if _, err := declarative.ParseProvider(sec, "v"); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
	if n < 2 {
		t.Errorf("found %d yaml model-provider blocks in the docs, want at least 2", n)
	}
	for _, f := range []string{"test/e2e/kind/model-provider.yaml", "hack/dev/model-provider-homelab.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		sec := parseManifest(t, f, string(raw), declarative.TypeModelProvider)
		d, err := declarative.ParseProvider(sec, "v")
		if err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if len(d.Models) == 0 || len(d.Warnings) > 0 {
			t.Errorf("%s: declare at least one model, without the removed rates (%v)", f, d.Warnings)
		}
	}
}

// TestDocumentedMCPServerSecretsParse keeps the mcp-server manifests in the docs valid.
func TestDocumentedMCPServerSecretsParse(t *testing.T) {
	root := filepath.Join("..", "..")
	block := regexp.MustCompile("(?s)```yaml mcp-server\n(.*?)```")
	f := "docs/site/src/content/docs/guides/mcp-servers.md"
	raw, err := os.ReadFile(filepath.Join(root, f))
	if err != nil {
		t.Fatal(err)
	}
	found := block.FindAllStringSubmatch(string(raw), -1)
	if len(found) == 0 {
		t.Fatalf("%s has no yaml mcp-server block", f)
	}
	for _, m := range found {
		sec := parseManifest(t, f, m[1], declarative.TypeMCPServer)
		if _, err := declarative.ParseMCPServer(sec, "v"); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
