// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// brokenHarnessImage is the image spec 8 of the quickstart points the harness at. Nobody can pull it.
const brokenHarnessImage = "localhost:5001/koinon-escape/agent:nope"

var harnessImageLine = regexp.MustCompile(`(?m)^(\s+image: ).*/sandbox[:@].*$`)

// breakHarnessImage returns config.yaml with the harness image line replaced by brokenHarnessImage.
func breakHarnessImage(cfgYAML string) string {
	return harnessImageLine.ReplaceAllString(cfgYAML, "${1}"+brokenHarnessImage)
}

// configMapPatch is the merge patch that sets config.yaml of the koinon-config ConfigMap.
func configMapPatch(cfgYAML string) (string, error) {
	b, err := json.Marshal(map[string]any{"data": map[string]string{"config.yaml": cfgYAML}})
	return string(b), err
}

// Issue 1190: spec 8 changes the cluster-wide config, and the stories pass runs on the same install
// afterwards. Spec 8 must restore the config, so the story agents get a pullable image.
func TestBrokenHarnessImageIsRestorable(t *testing.T) {
	orig := "harness:\n  image: localhost:5001/koinon-e1/sandbox:e2e\n  other: kept\n"
	broken := breakHarnessImage(orig)
	if !strings.Contains(broken, brokenHarnessImage) || broken == orig {
		t.Fatalf("the image line was not replaced: %q", broken)
	}
	p, err := configMapPatch(orig)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Data map[string]string }
	if err := json.Unmarshal([]byte(p), &got); err != nil || got.Data["config.yaml"] != orig {
		t.Fatalf("the restore patch does not carry the original config: %s (%v)", p, err)
	}
}

// The spec that breaks the harness image must register the restore before it applies the patch.
func TestQuickstartRestoresTheBrokenHarnessImage(t *testing.T) {
	src, err := os.ReadFile("quickstart_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	apply := strings.Index(text, `"patch", "cm", "koinon-config"`)
	restore := strings.Index(text, "restoreHarnessConfig(")
	if apply < 0 {
		t.Fatal("spec 8 no longer patches koinon-config")
	}
	if restore < 0 || restore > apply {
		t.Fatal("spec 8 changes the cluster-wide harness image and must DeferCleanup its restore (restoreHarnessConfig) before the change (issue 1190)")
	}
}

// restoreHarnessConfig returns the cleanup that puts the original config.yaml back, removes the wait cap
// and waits for the reconciler and agentd to roll out again. Call DeferCleanup with it BEFORE the patch.
func restoreHarnessConfig(kc func(args ...string) string, orig string) func() {
	return func() {
		p, err := configMapPatch(orig)
		if err != nil {
			panic(err)
		}
		kc("-n", "koinon", "patch", "cm", "koinon-config", "--type", "merge", "-p", p)
		kc("-n", "koinon", "set", "env", "deploy/koinon-agentd", "KOINON_SANDBOX_WAIT_CAP-")
		kc("-n", "koinon", "rollout", "restart", "deploy/koinon-reconciler")
		kc("-n", "koinon", "rollout", "status", "deploy/koinon-reconciler", "--timeout=3m")
		kc("-n", "koinon", "rollout", "status", "deploy/koinon-agentd", "--timeout=3m")
	}
}
