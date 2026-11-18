// SPDX-License-Identifier: Apache-2.0

package harness_test

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/templates"
)

func TestTableMatchesTemplates(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range harness.All() {
		if seen[d.Name] {
			t.Errorf("duplicate harness %q", d.Name)
		}
		seen[d.Name] = true
		if d.EntryScript == "" || len(d.DefaultBinaries) == 0 || d.Proto == 0 {
			t.Errorf("harness %q is missing an entry script, binaries or proto value", d.Name)
		}
		if d.PluginInstalls != (d.PluginInstallsArg != "") {
			t.Errorf("harness %q: PluginInstalls and PluginInstallsArg disagree", d.Name)
		}
		if d.NoWebToolsArg != "" && !strings.HasPrefix(d.NoWebToolsArg, "--") {
			t.Errorf("harness %q: NoWebToolsArg %q is not a switch", d.Name, d.NoWebToolsArg)
		}
		if got, ok := harness.FromProto(d.Proto); !ok || got.Name != d.Name {
			t.Errorf("harness %q does not round-trip through its proto value", d.Name)
		}
	}
	if len(templates.AllHarnesses) != len(harness.All()) {
		t.Errorf("templates.AllHarnesses = %v, descriptors = %d", templates.AllHarnesses, len(harness.All()))
	}
}

// The substrate driver wraps every harness Exec in the guest launcher; the prefix must end in "--"
// so the launcher takes the rest as the payload, and its policy must be the one of the tier: the vm
// tier requires Landlock, the gVisor tiers do not (ADR 0120 section 4).
func TestLauncherPrefixShape(t *testing.T) {
	want := map[harness.Tier]string{
		harness.TierVM:       "/etc/koinon/guest-launcher/policy-vm.json",
		harness.TierHardened: "/etc/koinon/guest-launcher/policy-gvisor.json",
		harness.TierDev:      "/etc/koinon/guest-launcher/policy-gvisor.json",
	}
	for _, d := range harness.All() {
		for tier, policy := range want {
			p, ok := d.LauncherPrefix(tier)
			if !ok || len(p) != 4 || p[0] != "/usr/local/bin/koinon-guest-launcher" || p[1] != "--policy" || p[2] != policy || p[3] != "--" {
				t.Errorf("harness %q tier %q: LauncherPrefix = %v, %v; want the launcher, --policy %s, --", d.Name, tier, p, ok, policy)
			}
		}
		if p, ok := d.LauncherPrefix("unknown"); ok || p != nil {
			t.Errorf("harness %q: an unknown tier got the prefix %v; it must fail closed", d.Name, p)
		}
	}
}

func TestOpenCodeDeclaresItsTools(t *testing.T) {
	d, ok := harness.Lookup("opencode")
	if !ok {
		t.Fatal("no opencode harness")
	}
	for _, tool := range []string{"rg", "fd", "jq", "yq", "ast-grep", "duckdb", "sqlite3", "pdftotext", "pdfinfo", "python3", "python", "file", "git"} {
		found := false
		for _, have := range d.Tools {
			found = found || have == tool
		}
		if !found {
			t.Errorf("opencode does not declare %q", tool)
		}
	}
	d.Tools[0] = "changed"
	if again, _ := harness.Lookup("opencode"); again.Tools[0] == "changed" {
		t.Error("Lookup shares the Tools slice with the table")
	}
}
