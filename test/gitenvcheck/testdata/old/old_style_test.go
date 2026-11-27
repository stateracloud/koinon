// SPDX-License-Identifier: Apache-2.0

// This file is a fixture for test/gitenvcheck. It is not compiled (the go tool skips testdata).
// It shows the old style that the check must find: git with the real environment.

package old_test

import (
	"os"
	"os/exec"
)

func runGit(repo string) error {
	c := exec.Command("git", "-C", repo, "status")
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return c.Run()
}
