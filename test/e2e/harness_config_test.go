// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
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
	restore := strings.Index(text, "restoreHarnessConfig(cctx")
	if apply < 0 {
		t.Fatal("spec 8 no longer patches koinon-config")
	}
	if restore < 0 || restore > apply {
		t.Fatal("spec 8 changes the cluster-wide harness image and must DeferCleanup its restore (restoreHarnessConfig with its own SpecContext) before the change (issue 1190)")
	}
}

// restoreHarnessConfig puts the original config.yaml back, removes the wait cap and waits for the
// reconciler and agentd to roll out again. run executes one kubectl command. Every step runs, and the
// errors are joined. The caller passes the context of its own cleanup node: the context of the spec is
// cancelled when the spec returns, before DeferCleanup runs.
func restoreHarnessConfig(ctx context.Context, run func(ctx context.Context, args ...string) error, orig string) error {
	p, err := configMapPatch(orig)
	if err != nil {
		return err
	}
	return errors.Join(
		run(ctx, "-n", "koinon", "patch", "cm", "koinon-config", "--type", "merge", "-p", p),
		run(ctx, "-n", "koinon", "set", "env", "deploy/koinon-agentd", "KOINON_SANDBOX_WAIT_CAP-"),
		run(ctx, "-n", "koinon", "rollout", "restart", "deploy/koinon-reconciler"),
		run(ctx, "-n", "koinon", "rollout", "status", "deploy/koinon-reconciler", "--timeout=3m"),
		run(ctx, "-n", "koinon", "rollout", "status", "deploy/koinon-agentd", "--timeout=3m"),
	)
}

func TestRestoreHarnessConfigRunsEveryStep(t *testing.T) {
	var calls []string
	run := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(calls) == 2 {
			return errors.New("set env failed")
		}
		return nil
	}
	err := restoreHarnessConfig(context.Background(), run, "image: orig\n")
	if err == nil || !strings.Contains(err.Error(), "set env failed") {
		t.Fatalf("the error is lost: %v", err)
	}
	if len(calls) != 5 {
		t.Fatalf("a failed step skipped the rest: %v", calls)
	}
	if !strings.Contains(calls[0], "patch cm koinon-config") || !strings.Contains(calls[0], "image: orig") ||
		!strings.Contains(calls[1], "KOINON_SANDBOX_WAIT_CAP-") || !strings.Contains(calls[2], "rollout restart deploy/koinon-reconciler") {
		t.Fatalf("unexpected steps: %v", calls)
	}
}
