// SPDX-License-Identifier: Apache-2.0

// Package gitenv gives a test an isolated environment for the git processes it starts.
//
// A git process that inherits os.Environ() reads and can write the HOME and the global and system
// git configuration of the person or the runner (credential helpers, hooks, maintenance). A test
// that starts git must use Env or Command instead (#1427). The check in test/gitenvcheck fails for
// a test file that runs git without this package.
package gitenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// Env returns the environment for a git process in a test. home is a temporary directory of the
// test: HOME and XDG_CONFIG_HOME point into it. The global and the system git configuration are
// off, and git never prompts. extra entries (KEY=value) come last and win.
func Env(home string, extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	// A sandbox can allow writes only under TMPDIR, so pass it on.
	if v, ok := os.LookupEnv("TMPDIR"); ok {
		env = append(env, "TMPDIR="+v)
	}
	return append(env, extra...)
}

// Command returns a git command in dir with the environment of Env. home is a temporary
// directory of the test; it can be dir itself.
func Command(ctx context.Context, home, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary; the test gives the arguments
	cmd.Dir = dir
	cmd.Env = Env(home)
	return cmd
}
