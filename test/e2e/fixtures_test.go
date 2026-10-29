// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
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
// default is the first served, priced model by provider name, then model name (agentmodel.OrgDefault,
// ListServedCatalog's ORDER BY). The e2e declares a provider the quickstart does not
// (kind/model-provider-anthropic.yaml), and its models are priced by the default price list
// (claude-haiku-5-5) or by a later step (claude-e2e in 6d). If its name sorted first, one of them would
// be the org default and the quickstart's run would get another script than repo-helper's, which
// makes the tool call step 4 parks on (rc.8: "anthropic-fake" did, and step 4 ran claude-haiku-5-5's
// script to success). This check runs offline with `go test ./...`; the e2e suite itself is skipped
// there.
func TestE2EProvidersKeepTheQuickstartModelTheOrgDefault(t *testing.T) {
	quickstart, models := readProvider(t, "model-provider.yaml")
	if len(models) == 0 || models[0] != "repo-helper" {
		t.Fatalf("the quickstart provider %q must declare repo-helper first, got %v", quickstart, models)
	}
	extra, _ := readProvider(t, "model-provider-anthropic.yaml")
	if extra == "" || extra <= quickstart {
		t.Fatalf("the e2e-only provider %q must sort after the quickstart's %q, or its models become the org default the quickstart steps run on", extra, quickstart)
	}
}
