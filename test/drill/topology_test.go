// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

func TestTheHookInputsAreChecked(t *testing.T) {
	b, err := os.ReadFile("topology/lab.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	top, err := drill.LoadTopology("topology/lab.example.yaml", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if top.NamespaceHook == "" || top.NamespaceLabels["example.invalid/secret-store"] != "true" {
		t.Errorf("the lab example should show the hook and the label: %+v", top)
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
		"idp image without a digest": func(s string) string {
			return regexp.MustCompile(`(fakeIdpImage: \S+?)@sha256:\w+`).ReplaceAllString(s, "$1")
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

func TestTheOIDCValuesPointTheChartAtTheDrillsOwnProvider(t *testing.T) {
	gw := drill.Gateway{Namespace: "koinon-drill-os-src-abc123", Service: "openshell", ClientTLSSecret: "c"}
	for port, redirect := range map[int]string{
		drill.OIDCSourcePort: "redirectUrl: http://127.0.0.1:18181/auth/oidc/callback",
		drill.OIDCTargetPort: "redirectUrl: http://127.0.0.1:18182/auth/oidc/callback",
	} {
		on, err := drill.ValuesOverlay("koinon-drill-fakes-abc123", gw, "drill-src-abc123", false, "", nil, port)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"issuerUrl: http://dex.koinon-drill-fakes-abc123.svc.cluster.local:5556/dex", "clientId: koinon-drill", redirect,
			"koinon-admins: admin", "koinon-users: member", "- groups",
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
}

func TestTheFakeMCPServerIsAnInputAndItsHostIsAllowed(t *testing.T) {
	top, err := drill.LoadTopology("topology/kind.yaml", "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	if top.FakeMCPImage == "" {
		t.Fatal("the kind topology names the fake MCP image")
	}
	top.FakeMCPImage = ""
	if err := top.Validate(); err == nil || !strings.Contains(err.Error(), "fakeMcpImage") {
		t.Fatalf("a topology without fakeMcpImage must be refused, got %v", err)
	}
	m := drill.FakeMCPManifest("koinon-drill-fakes-a1b2c3", "a1b2c3", "localhost:5001/koinon-fakemcp:e2e")
	for _, want := range []string{"name: fakemcp", drill.FakeMCPKey, drill.RunLabel + `: "a1b2c3"`} {
		if !strings.Contains(m, want) {
			t.Errorf("the fake MCP manifest lacks %q", want)
		}
	}
	if !strings.Contains(drill.FakeMCPURL("koinon-drill-fakes-a1b2c3"), "fakemcp.koinon-drill-fakes-a1b2c3.svc:8080/mcp") {
		t.Error("the fake MCP URL names the Service in the fakes namespace")
	}
}

// publishedFakes maps each Topology image field the release publishes to the image name that
// hack/release-test-images.sh pushes for it.
var publishedFakes = map[string]string{
	"FakeModelImage": "koinon-fakemodel",
	"FakeMCPImage":   "koinon-fakemcp",
}

// unpublishedImages are image fields the release deliberately does not publish; they are allowed even when
// Validate() requires them.
var unpublishedImages = map[string]string{
	// The S3 server for home checkpoints is an upstream image (SeaweedFS), pinned by digest in the topology.
	"ObjectStoreImage": "upstream image, pinned by digest; not built by this repository",
	// The drill's OIDC provider is upstream Dex, pinned by digest; a lab mirrors that digest (ADR 0109, amendment 2026-11-13).
	"FakeIdPImage": "upstream image, pinned by digest; not built by this repository",
}

// releaseFakes reads the fakes=(...) array of hack/release-test-images.sh, on one line or several.
func releaseFakes(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../hack/release-test-images.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^fakes=\(([^)]*)\)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("hack/release-test-images.sh: no fakes=(...) array found")
	}
	var out []string
	for _, line := range strings.Split(m[1], "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out = append(out, strings.Fields(line)...)
	}
	return out
}

// imageFields returns pointers to every string field named *Image, at any depth, keyed by field name.
func imageFields(v reflect.Value, into map[string]*string) {
	for i := 0; i < v.NumField(); i++ {
		f, sf := v.Field(i), v.Type().Field(i)
		switch {
		case f.Kind() == reflect.Struct:
			imageFields(f, into)
		case f.Kind() == reflect.String && strings.HasSuffix(sf.Name, "Image"):
			into[sf.Name] = f.Addr().Interface().(*string)
		}
	}
}

// The drill topology must not require an image the release does not publish (#952). Two checks: every field
// in publishedFakes names an image hack/release-test-images.sh pushes, and every image field Validate()
// rejects when blank (today only fakeMcpImage) is published or allowlisted.
func TestEveryImageTheDrillRequiresIsPublishedOrAllowlisted(t *testing.T) {
	top, err := drill.LoadTopology("topology/kind.yaml", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]*string{}
	imageFields(reflect.ValueOf(top).Elem(), fields)
	if len(fields) == 0 {
		t.Fatal("no *Image fields found in Topology")
	}
	if err := top.Validate(); err != nil {
		t.Fatalf("topology/kind.yaml must validate before fields are blanked: %v", err)
	}

	published := map[string]bool{}
	for _, f := range releaseFakes(t) {
		published[f] = true
	}
	for field, img := range publishedFakes {
		if !published[img] {
			t.Errorf("%s maps to %s, which hack/release-test-images.sh does not publish", field, img)
		}
		if _, ok := fields[field]; !ok {
			t.Errorf("publishedFakes names %s, which is not a Topology image field", field)
		}
	}

	for name, p := range fields {
		saved := *p
		*p = ""
		verr := top.Validate()
		*p = saved
		if verr == nil {
			continue // optional field
		}
		_, isPub := publishedFakes[name]
		_, isAllowed := unpublishedImages[name]
		if !isPub && !isAllowed {
			t.Errorf("Validate() requires %s, but no published image maps to it and it is not allowlisted: publish it in hack/release-test-images.sh or allowlist it with a reason", name)
		}
	}
	for name := range unpublishedImages {
		if _, ok := fields[name]; !ok {
			t.Errorf("allowlist names %s, which is not a Topology image field", name)
		}
	}
}
