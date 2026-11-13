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

func TestTheNamespaceManifestCarriesTheRunLabelAndTheOperatorsLabels(t *testing.T) {
	got := drill.NamespaceManifest("koinon-drill-src-abc123", "abc123", map[string]string{"lab.example.invalid/store": "true", "a.example.invalid/x": "y"})
	for _, want := range []string{
		"name: koinon-drill-src-abc123\n",
		drill.RunLabel + ": \"abc123\"\n",
		"    a.example.invalid/x: \"y\"\n    lab.example.invalid/store: \"true\"\n", // sorted
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest lacks %q:\n%s", want, got)
		}
	}
	if plain := drill.NamespaceManifest("koinon-drill-src-abc123", "abc123", nil); strings.Count(plain, "\n    ") != 1 {
		t.Errorf("no operator labels, want the name and the run label only:\n%s", plain)
	}
}

func TestTheHookAndOIDCInputsAreChecked(t *testing.T) {
	b, err := os.ReadFile("topology/lab.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	top, err := drill.LoadTopology("topology/lab.example.yaml", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if top.NamespaceHook == "" || top.NamespaceLabels["example.invalid/secret-store"] != "true" || top.OIDC == nil {
		t.Errorf("the lab example should show the hook, the label and the OIDC inputs: %+v", top)
	}
	if got := drill.OIDCRedirect(drill.OIDCSourcePort); got != "http://127.0.0.1:18181/auth/oidc/callback" {
		t.Errorf("source redirect: %q", got)
	}
	if got := drill.OIDCRedirect(drill.OIDCTargetPort); got != "http://127.0.0.1:18182/auth/oidc/callback" {
		t.Errorf("target redirect: %q", got)
	}
	dir := t.TempDir()
	for name, mod := range map[string]func(string) string{
		"the run label": func(s string) string { return s + "\nnamespaceLabels: {" + drill.RunLabel + ": x}\n" },
		"bad label key": func(s string) string { return strings.Replace(s, "example.invalid/secret-store:", "'-bad key':", 1) },
		"bad label value": func(s string) string {
			return strings.Replace(s, "secret-store: \"true\"", "secret-store: \"not valid!\"", 1)
		},
		"http issuer":     func(s string) string { return strings.Replace(s, "issuerUrl: https://", "issuerUrl: http://", 1) },
		"no client":       func(s string) string { return strings.Replace(s, "clientId: koinon-drill", "clientId: \"\"", 1) },
		"one group":       func(s string) string { return strings.Replace(s, "group: koinon-users", "group: koinon-admins", 1) },
		"no member group": func(s string) string { return strings.Replace(s, "group: koinon-users", "group: \"\"", 1) },
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

func TestTheOIDCValuesReachTheChartOnlyWhenSet(t *testing.T) {
	gw := drill.Gateway{Namespace: "koinon-drill-os-src-abc123", Service: "openshell", ClientTLSSecret: "c"}
	oidc := &drill.OIDC{
		IssuerURL: "https://idp.example.invalid/realms/drill", ClientID: "koinon-drill",
		Admin: drill.Persona{Group: "koinon-admins"}, Member: drill.Persona{Group: "koinon-users"},
	}
	for port, redirect := range map[int]string{
		drill.OIDCSourcePort: "redirectUrl: http://127.0.0.1:18181/auth/oidc/callback",
		drill.OIDCTargetPort: "redirectUrl: http://127.0.0.1:18182/auth/oidc/callback",
	} {
		on, err := drill.ValuesOverlay("koinon-drill-fakes-abc123", gw, "drill-src-abc123", false, "", nil, oidc, port)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"issuerUrl: https://idp.example.invalid/realms/drill", "clientId: koinon-drill", redirect,
			"koinon-admins: admin", "koinon-users: member",
		} {
			if !strings.Contains(on, want) {
				t.Errorf("overlay for port %d lacks %q:\n%s", port, want, on)
			}
		}
		// A public client: no client secret anywhere.
		if strings.Contains(on, "clientSecret") || strings.Contains(on, "existingSecret") {
			t.Errorf("the OIDC client is public, but the overlay names a secret:\n%s", on)
		}
	}
	off, _ := drill.ValuesOverlay("koinon-drill-fakes-abc123", gw, "drill-src-abc123", false, "", nil, nil, 0)
	if strings.Contains(off, "oidc") {
		t.Errorf("no OIDC input, but the overlay has it:\n%s", off)
	}
}

func TestThePersonaEnvNamesAreFixed(t *testing.T) {
	if got := drill.OIDCEnv(drill.PersonaAdmin, "PRIVATE_KEY"); got != "KOINON_DRILL_OIDC_ADMIN_PRIVATE_KEY" {
		t.Errorf("env name: %q", got)
	}
}
