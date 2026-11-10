// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"os"
	"path/filepath"
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

// TestE2EAndDevProviderManifestsParse keeps the model-provider manifests of the e2e test and of the dev
// profiles valid for the importer, so a change to the Secret's format cannot leave one behind. The docs
// site (stateracloud/koinon-docs, ADR 0108) shows manifests of its own; they are not read here.
func TestE2EAndDevProviderManifestsParse(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, f := range []string{"test/e2e/kind/model-provider.yaml", "hack/dev/model-provider.example.yaml"} {
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
