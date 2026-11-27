// SPDX-License-Identifier: Apache-2.0

package gitenv_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/testing/gitenv"
)

func TestEnvIsolatesHomeAndConfig(t *testing.T) {
	home := t.TempDir()
	env := strings.Join(gitenv.Env(home, "GIT_CONFIG_GLOBAL=override"), "\n")
	for _, want := range []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
	if strings.Contains(env, os.Getenv("HOME")+"\n") && os.Getenv("HOME") != home {
		t.Error("env carries the real HOME")
	}
}

func TestCommandIgnoresGlobalConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := gitenv.Command(context.Background(), home, home, "config", "--global", "--get", "user.name").CombinedOutput()
	if strings.Contains(string(out), "leaked") {
		t.Errorf("git read the home config: %s", out)
	}
}
