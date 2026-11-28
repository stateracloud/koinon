// SPDX-License-Identifier: Apache-2.0

package main_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// TestImagePolicyDirReadableByNonRoot builds the Dockerfile of this command on a small base with stub binaries
// and checks the policy files from a non-root user (issue #1391). A `COPY --chmod=0644` of files into a new
// directory makes BuildKit give the directory the same mode, so the directory loses its x bit and a non-root
// reader gets "permission denied" (the launcher exits 64). The directory must be 0755 and the files 0644.
func TestImagePolicyDirReadableByNonRoot(t *testing.T) {
	testenv.RequireT(t, testenv.Docker)

	ctx := t.TempDir()
	for _, f := range []string{"Dockerfile", "policy-vm.json", "policy-gvisor.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctx, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	launcher := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		dir := filepath.Join(launcher, "linux-"+arch)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, bin := range []string{"koinon-guest-launcher", "koinon-guest-front"} {
			if err := os.WriteFile(filepath.Join(dir, bin), []byte("stub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The tag is unique per run: t.TempDir() names repeat ("001") across processes on one Docker daemon.
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	tag := "koinon-guest-launcher-image-test:" + hex.EncodeToString(suffix)
	build := exec.CommandContext(t.Context(), "docker", "build", "--quiet", "--build-context", "flavor=docker-image://busybox:1.37",
		"--build-context", "launcher="+launcher, "--tag", tag, ctx)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rmi", "--force", tag).Run() })

	const dir = "/etc/koinon/guest-launcher"
	script := `set -e
for f in policy-vm.json policy-gvisor.json; do cat ` + dir + `/$f >/dev/null; done
stat -c '%a' ` + dir + ` ` + dir + `/policy-vm.json ` + dir + `/policy-gvisor.json`
	run := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--user", "10001:10001", "--entrypoint", "sh", tag, "-c", script)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("a non-root user cannot read the policy files: %v\n%s", err, out)
	}
	if got, want := strings.Fields(string(out)), []string{"755", "644", "644"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("modes of the directory and the two policy files = %v, want %v", got, want)
	}
}
