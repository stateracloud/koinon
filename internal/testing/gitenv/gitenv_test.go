// SPDX-License-Identifier: Apache-2.0

package gitenv_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/gitenv"
)

var _ = Describe("Env", func() {
	It("points HOME to the given directory and turns the global and system config off", func() {
		home := GinkgoT().TempDir()
		env := gitenv.Env(home)
		Expect(env).To(ContainElements("HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0"))
		var homes []string
		for _, kv := range env {
			if strings.HasPrefix(kv, "HOME=") {
				homes = append(homes, kv)
			}
		}
		Expect(homes).To(Equal([]string{"HOME=" + home}))
	})

	It("puts the extra entries last, so that they win", func() {
		env := gitenv.Env(GinkgoT().TempDir(), "GIT_CONFIG_GLOBAL=override")
		Expect(env[len(env)-1]).To(Equal("GIT_CONFIG_GLOBAL=override"))
	})

	It("passes TMPDIR on when it is set", func() {
		home := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", "/tmp/koinon-gitenv-test")
		Expect(gitenv.Env(home)).To(ContainElement("TMPDIR=/tmp/koinon-gitenv-test"))
	})
})

var _ = Describe("Command", func() {
	It("does not read the global config of the home", func() {
		_, err := exec.LookPath("git")
		if err != nil {
			Skip("git not found")
		}
		home := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = leaked\n"), 0o600)).To(Succeed())
		out, _ := gitenv.Command(context.Background(), home, home, "config", "--get", "user.name").CombinedOutput()
		Expect(string(out)).NotTo(ContainSubstring("leaked"))
	})
})
