// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// linux says that the machine is Linux. The script uses GNU stat -c and mv -T and runs only in the Linux guest; it
// is not ported to BSD flags (#1519).
var linux = testenv.Check("linux", func() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the token script needs GNU stat and mv, and this machine is %s", runtime.GOOS)
	}
	return nil
})

// The token script runs in a real shell: the guest owns /tmp, so a link it plants at the token
// directory must not send the token into the home (#1086 part C2).
var _ = Describe("the actor token write script", func() {
	run := func(dir, file, in string) (string, error) {
		// The production command, with the directory and the file of a temporary root.
		c := codehost.WriteEgressTokenCommand()
		cmd := exec.Command(c[0], c[1], c[2], c[3], dir, file)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	BeforeEach(func() { testenv.Require(linux) })

	It("writes the file in a new private directory and replaces it on the next write", func() {
		dir := filepath.Join(GinkgoT().TempDir(), ".koinon-egress")
		file := filepath.Join(dir, "token.gitconfig")
		out, err := run(dir, file, "first")
		Expect(err).NotTo(HaveOccurred(), out)
		out, err = run(dir, file, "second")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(os.ReadFile(file)).To(BeEquivalentTo("second"))
		st, err := os.Stat(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()).To(Equal(os.FileMode(0o700)))
		left, err := filepath.Glob(filepath.Join(dir, ".git.*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(left).To(BeEmpty(), "no temporary file is left")
	})

	It("refuses a token directory that is a symbolic link, and writes nothing through it", func() {
		root := GinkgoT().TempDir()
		home := filepath.Join(root, "sandbox")
		Expect(os.Mkdir(home, 0o700)).To(Succeed())
		dir := filepath.Join(root, ".koinon-egress")
		Expect(os.Symlink(home, dir)).To(Succeed())
		out, err := run(dir, filepath.Join(dir, "token.gitconfig"), "koinon_test_not-a-real-token")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("is not a plain directory"))
		entries, err := os.ReadDir(home)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "the home holds no token")
	})

	It("refuses a token directory that is not private, and writes nothing in it", func() {
		dir := filepath.Join(GinkgoT().TempDir(), ".koinon-egress")
		Expect(os.Mkdir(dir, 0o700)).To(Succeed())
		Expect(os.Chmod(dir, 0o755)).To(Succeed())
		out, err := run(dir, filepath.Join(dir, "token.gitconfig"), "koinon_test_not-a-real-token")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("is not a plain directory of this user with mode 0700"))
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})

	It("replaces a symbolic link to a directory at the file path, and writes nothing through it", func() {
		root := GinkgoT().TempDir()
		home := filepath.Join(root, "sandbox")
		Expect(os.Mkdir(home, 0o700)).To(Succeed())
		dir := filepath.Join(root, ".koinon-egress")
		Expect(os.Mkdir(dir, 0o700)).To(Succeed())
		file := filepath.Join(dir, "token.gitconfig")
		Expect(os.Symlink(home, file)).To(Succeed())
		out, err := run(dir, file, "koinon_test_not-a-real-token")
		Expect(err).NotTo(HaveOccurred(), out)
		st, err := os.Lstat(file)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Mode().IsRegular()).To(BeTrue())
		entries, err := os.ReadDir(home)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "the home holds no token")
	})

	It("refuses a token directory that is a file", func() {
		dir := filepath.Join(GinkgoT().TempDir(), ".koinon-egress")
		Expect(os.WriteFile(dir, nil, 0o600)).To(Succeed())
		out, err := run(dir, filepath.Join(dir, "token.gitconfig"), "koinon_test_not-a-real-token")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("is not a plain directory"))
	})
})
