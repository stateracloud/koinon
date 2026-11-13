// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/test/drill"
)

func TestTheShippedTopologiesLoad(t *testing.T) {
	for file, want := range map[string]struct {
		profile string
		browser bool
	}{
		"topology/kind.yaml":        {drill.ProfileKind, false},
		"topology/lab.example.yaml": {drill.ProfileLab, true},
	} {
		top, err := drill.LoadTopology(file, "abc123")
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if top.Profile != want.profile || top.BrowserSignin != want.browser {
			t.Errorf("%s: profile %q browserSignin %v, want %q %v", file, top.Profile, top.BrowserSignin, want.profile, want.browser)
		}
	}
}

// The example is public and the lab's real topology is not: the example names nothing real.
func TestTheLabExampleNamesNothingReal(t *testing.T) {
	b, err := os.ReadFile("topology/lab.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, bad := range []string{"192.168.", "10.0.", "172.16.", ".lan", ".local", "procodus"} {
			if strings.Contains(line, bad) {
				t.Errorf("lab.example.yaml: %q looks like a real address or host (%s)", line, bad)
			}
		}
	}
}

func TestAProfileIsRequiredAndTheLabNeedsTheBrowser(t *testing.T) {
	b, err := os.ReadFile("topology/lab.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, mod := range map[string]func(string) string{
		"no profile":      func(s string) string { return strings.Replace(s, "profile: lab\n", "", 1) },
		"unknown profile": func(s string) string { return strings.Replace(s, "profile: lab", "profile: staging", 1) },
		"lab, no browser": func(s string) string { return strings.Replace(s, "browserSignin: true", "browserSignin: false", 1) },
		"lab, not in repo": func(s string) string {
			return strings.Replace(s, "namespace: koinon-drill-src-", "namespace: prod-", 1)
		},
	} {
		f := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		if err := os.WriteFile(f, []byte(mod(string(b))), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := drill.LoadTopology(f, "abc123"); err == nil {
			t.Errorf("%s: loaded, want an error", name)
		}
	}
}

func TestAnAbsolutePathIsKeptAndARelativeOneIsRootedInTheRepository(t *testing.T) {
	top := &drill.Topology{}
	if got := top.Path("/repo", "/etc/lab/values.yaml"); got != "/etc/lab/values.yaml" {
		t.Errorf("absolute: %q", got)
	}
	if got := top.Path("/repo", "charts/koinon"); got != "/repo/charts/koinon" {
		t.Errorf("relative: %q", got)
	}
}
