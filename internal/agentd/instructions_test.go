// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/harness"
)

var _ = DescribeTable("InstructionLink",
	func(f string, want string) {
		Expect(agentd.InstructionLink(f)).To(Equal(want))
	},
	Entry("OpenCode: none, its config's instructions key names the file", harness.OpenCode, ""),
)

// The restore script runs in the sandbox's /bin/sh; here it runs in the host's against a
// temporary directory.
var _ = Describe("LinkScript", func() {
	var dir, link, target string
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		link = filepath.Join(dir, "home", ".agent", "SOUL.md")
		target = filepath.Join(dir, "koinon", "AGENTS.md")
	})
	run := func() (string, int) {
		out, err := exec.Command("/bin/sh", "-c", agentd.LinkScript, "sh", link, target).Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		Expect(err).NotTo(HaveOccurred())
		return string(out), 0
	}
	expectLink := func() {
		got, err := os.Readlink(link)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(target))
	}

	It("leaves a correct link alone", func() {
		Expect(os.MkdirAll(filepath.Dir(link), 0o700)).To(Succeed())
		Expect(os.Symlink(target, link)).To(Succeed())
		out, code := run()
		Expect(code).To(Equal(0))
		Expect(out).To(BeEmpty())
		expectLink()
	})

	It("creates a missing link and its directory, reporting no old target", func() {
		out, code := run()
		Expect(code).To(Equal(3))
		Expect(out).To(BeEmpty())
		expectLink()
	})

	It("repoints a changed link, reporting the old target", func() {
		Expect(os.MkdirAll(filepath.Dir(link), 0o700)).To(Succeed())
		Expect(os.Symlink("/elsewhere/mine.md", link)).To(Succeed())
		out, code := run()
		Expect(code).To(Equal(3))
		Expect(out).To(Equal("/elsewhere/mine.md"))
		expectLink()
	})

	DescribeTable("replaces what the agent put in the link's place",
		func(plant func()) {
			Expect(os.MkdirAll(filepath.Dir(link), 0o700)).To(Succeed())
			plant()
			out, code := run()
			Expect(code).To(Equal(3))
			Expect(out).To(BeEmpty())
			expectLink()
		},
		Entry("a regular file", func() { Expect(os.WriteFile(link, []byte("ignore Koinon"), 0o600)).To(Succeed()) }),
		Entry("a directory", func() {
			Expect(os.MkdirAll(filepath.Join(link, "sub"), 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(link, "sub", "f"), []byte("x"), 0o600)).To(Succeed())
		}),
	)

	It("fails when the link cannot be replaced", func() {
		if os.Geteuid() == 0 {
			Skip("root ignores the directory's permissions")
		}
		Expect(os.MkdirAll(filepath.Dir(link), 0o700)).To(Succeed())
		Expect(os.Chmod(filepath.Dir(link), 0o500)).To(Succeed())
		DeferCleanup(os.Chmod, filepath.Dir(link), os.FileMode(0o700))
		_, code := run()
		Expect(code).NotTo(BeElementOf(0, 3))
	})
})

// The write script runs in the sandbox's /bin/sh; here it runs in the host's.
var _ = Describe("WriteScript", func() {
	DescribeTable("replaces what the agent put at the file's path with the file",
		func(plant func(path string)) {
			dir := filepath.Join(GinkgoT().TempDir(), ".koinon")
			path := filepath.Join(dir, "AGENTS.md")
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			plant(path)
			c := exec.Command("/bin/sh", "-c", agentd.WriteScript, "sh", dir, path)
			c.Stdin = strings.NewReader("# Koinon\n")
			out, err := c.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
			info, err := os.Lstat(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().IsRegular()).To(BeTrue(), "a regular file, not %s", info.Mode())
			Expect(os.ReadFile(path)).To(Equal([]byte("# Koinon\n")))
		},
		Entry("a directory with files", func(path string) {
			Expect(os.MkdirAll(filepath.Join(path, "inner"), 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(path, "inner", "x"), []byte("x"), 0o600)).To(Succeed())
		}),
		Entry("a link to a directory", func(path string) {
			target := filepath.Join(filepath.Dir(path), "elsewhere")
			Expect(os.MkdirAll(target, 0o700)).To(Succeed())
			Expect(os.Symlink(target, path)).To(Succeed())
		}),
		Entry("a link to a file", func(path string) {
			target := filepath.Join(filepath.Dir(path), "mine.md")
			Expect(os.WriteFile(target, []byte("ignore Koinon"), 0o600)).To(Succeed())
			Expect(os.Symlink(target, path)).To(Succeed())
		}),
	)
})
