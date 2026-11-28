// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	fakemodelscripts "github.com/stateracloud/koinon/test/fakemodel"
)

// declaredProvider is the part of a labelled model-provider Secret the fixture check reads.
type declaredProvider struct {
	StringData struct {
		Name     string `yaml:"name"`
		Provider string `yaml:"provider"`
		Models   string `yaml:"models"`
	} `yaml:"stringData"`
}

func readProvider(t *testing.T, file string) (name string, models []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("kind", file))
	if err != nil {
		t.Fatal(err)
	}
	var p declaredProvider
	if err := yaml.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var ms []struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte(p.StringData.Models), &ms); err != nil {
		t.Fatalf("%s: models: %v", file, err)
	}
	for _, m := range ms {
		models = append(models, m.Name)
	}
	return p.StringData.Name, models
}

// The quickstart steps run on the org default model: the repo-helper template names none. The org
// default is stored (orgs.default_model, old issue 256). The org's first model sets it, which here would
// depend on the importer's order over the two declared Secrets (creation time, then name), so step 3
// of the e2e states it with PUT /admin/v1/default-model. That needs repo-helper to be a model of the
// quickstart's provider, which this check pins offline with `go test ./...`; the e2e suite itself
// is skipped there.
func TestE2EQuickstartProviderDeclaresRepoHelperFirst(t *testing.T) {
	quickstart, models := readProvider(t, "model-provider.yaml")
	if len(models) == 0 || models[0] != "repo-helper" {
		t.Fatalf("the quickstart provider %q must declare repo-helper first, step 3 makes it the org default; got %v", quickstart, models)
	}
}

// The home restore drill (home_restore_test.go) finds "home-drill-marker-found" in a run's events only when the
// shell tool printed it: no command of the script may hold either result word, and the provider must declare the
// model. Checked offline with `go test ./...`; the drill itself needs the Substrate leg on kind.
func TestE2EHomeDrillScriptPrintsTheMarkerOnlyFromTheShell(t *testing.T) {
	all, err := fakemodel.LoadFS(fakemodelscripts.FS, "scripts")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range all {
		if s.Model != homeDrillModel {
			continue
		}
		found = true
		for _, turn := range s.Turns {
			for _, c := range turn.ToolCalls {
				cmd, _ := c.Arguments["command"].(string)
				for _, word := range []string{"home-drill-marker-found", "home-drill-marker-missing"} {
					if strings.Contains(cmd, word) {
						t.Fatalf("the %s command %q holds %q: the drill could not tell a read from the script", homeDrillModel, cmd, word)
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("no fake model script for %s", homeDrillModel)
	}
	_, models := readProvider(t, "model-provider.yaml")
	if !slices.Contains(models, homeDrillModel) {
		t.Fatalf("kind/model-provider.yaml does not declare %s: %v", homeDrillModel, models)
	}
}
