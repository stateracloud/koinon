// SPDX-License-Identifier: Apache-2.0

package flavor_test

import (
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
		if got, ok := flavor.FromProto(d.Proto); !ok || got.Name != d.Name {
			t.Errorf("flavor %q does not round-trip through its proto value", d.Name)
		}
	}
	if len(templates.AllFlavors) != len(flavor.All()) {
		t.Errorf("templates.AllFlavors = %v, descriptors = %d", templates.AllFlavors, len(flavor.All()))
	}
}
