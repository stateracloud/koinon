// SPDX-License-Identifier: Apache-2.0

package flavor_test

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/flavor"
	"github.com/stateracloud/koinon/templates"
)

func TestTableMatchesTemplates(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range flavor.All() {
		if seen[string(d.Name)] {
			t.Errorf("duplicate flavor %q", d.Name)
		}
		seen[string(d.Name)] = true
		if d.EntryScript == "" || len(d.DefaultBinaries) == 0 || d.Proto == 0 {
			t.Errorf("flavor %q is missing an entry script, binaries or proto value", d.Name)
		}
		if d.PluginInstalls != (d.PluginInstallsArg != "") {
			t.Errorf("flavor %q: PluginInstalls and PluginInstallsArg disagree", d.Name)
		}
		if d.NoWebToolsArg != "" && !strings.HasPrefix(d.NoWebToolsArg, "--") {
			t.Errorf("flavor %q: NoWebToolsArg %q is not a switch", d.Name, d.NoWebToolsArg)
		}
		if got, ok := flavor.FromProto(d.Proto); !ok || got.Name != d.Name {
			t.Errorf("flavor %q does not round-trip through its proto value", d.Name)
		}
	}
	if len(templates.AllFlavors) != len(flavor.All()) {
		t.Errorf("templates.AllFlavors = %v, descriptors = %d", templates.AllFlavors, len(flavor.All()))
	}
}

func TestOpenCodeDeclaresItsTools(t *testing.T) {
	d, ok := flavor.Lookup("opencode")
	if !ok {
		t.Fatal("no opencode flavor")
	}
	for _, tool := range []string{"rg", "fd", "jq", "yq", "ast-grep", "duckdb", "sqlite3", "pdftotext", "python3", "git"} {
		found := false
		for _, have := range d.Tools {
			found = found || have == tool
		}
		if !found {
			t.Errorf("opencode does not declare %q", tool)
		}
	}
	d.Tools[0] = "changed"
	if again, _ := flavor.Lookup("opencode"); again.Tools[0] == "changed" {
		t.Error("Lookup shares the Tools slice with the table")
	}
}
