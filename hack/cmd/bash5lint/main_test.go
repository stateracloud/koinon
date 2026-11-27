// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/gitenv"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestBash5lint(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "bash5lint")
}

func write(root, rel, content string) {
	p := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
	Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
}

func lint(root string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code, err := run(root, &o, &e)
	Expect(err).NotTo(HaveOccurred())
	return code, o.String(), e.String()
}

var _ = Describe("checkScript", func() {
	It("accepts the guard after header comments and blank lines", func() {
		msg, counted := checkScript("hack/a.sh", "#!/usr/bin/env bash\n# SPDX\n# header\n\n"+guard+"\nset -e\n")
		Expect(msg).To(BeEmpty())
		Expect(counted).To(BeTrue())
	})
	It("rejects a command above the guard", func() {
		msg, _ := checkScript("hack/a.sh", "#!/usr/bin/env bash\ndeclare -A m\n"+guard+"\n")
		Expect(msg).To(Equal("the first command is not the bash 5 guard"))
	})
	It("rejects a drifted guard", func() {
		msg, _ := checkScript("hack/a.sh", "#!/usr/bin/env bash\n[ \"${BASH_VERSINFO:-0}\" -ge 4 ] || exit 2\n")
		Expect(msg).NotTo(BeEmpty())
	})
	It("rejects a file with a shebang and no command", func() {
		msg, _ := checkScript("hack/a.sh", "#!/usr/bin/env bash\n# only comments\n")
		Expect(msg).NotTo(BeEmpty())
	})
	It("skips sh scripts, files without a shebang and an empty file", func() {
		for _, c := range []string{"#!/bin/sh\necho hi\n", "x=1\n", ""} {
			msg, counted := checkScript("hack/a.sh", c)
			Expect(msg).To(BeEmpty())
			Expect(counted).To(BeFalse())
		}
	})
	DescribeTable("rejects any other bash shebang",
		func(sb string) {
			msg, counted := checkScript("hack/a.sh", sb+"\n"+guard+"\n")
			Expect(msg).To(Equal("bash shebang '" + sb + "' (use #!/usr/bin/env bash)"))
			Expect(counted).To(BeFalse())
		},
		Entry(nil, "#!/bin/bash"),
		Entry(nil, "#!/usr/bin/env -S bash -e"),
		Entry(nil, "#!/usr/local/bin/bash"),
	)
	Describe("a registered hook", func() {
		const hook = "hack/hooks/bash-read-check.sh"
		head := "#!/usr/bin/env bash\n"
		It("passes with the opt-in line, then the fail-open guard", func() {
			msg, _ := checkScript(hook, head+"# c\n"+hookGate+"\n\n# c\n"+hookGuard+"\n")
			Expect(msg).To(BeEmpty())
		})
		It("fails without the opt-in line", func() {
			msg, _ := checkScript(hook, head+hookGuard+"\n")
			Expect(msg).NotTo(BeEmpty())
		})
		It("fails with the opt-in line second", func() {
			msg, _ := checkScript(hook, head+hookGuard+"\n"+hookGate+"\n")
			Expect(msg).NotTo(BeEmpty())
		})
		It("fails with the exit-2 guard", func() {
			msg, _ := checkScript(hook, head+hookGate+"\n"+guard+"\n")
			Expect(msg).NotTo(BeEmpty())
		})
	})
	DescribeTable("rejects the hook variant outside the registered hooks",
		func(rel string) {
			msg, _ := checkScript(rel, "#!/usr/bin/env bash\n"+hookGate+"\n"+hookGuard+"\n")
			Expect(msg).NotTo(BeEmpty())
		},
		Entry(nil, "hack/y.sh"),
		Entry(nil, "hack/hooks/sub/evil-check.sh"),
		Entry(nil, "test/hack/hooks/x-check.sh"),
		Entry(nil, "hack/hooks/notahook-check.sh"),
		Entry(nil, "hack/hooks/z_test.sh"),
	)
})

var _ = Describe("run", func() {
	It("passes a guarded tree and reports the count", func() {
		root := GinkgoT().TempDir()
		write(root, "hack/a.sh", "#!/usr/bin/env bash\n# h\n\n"+guard+"\nset -e\n")
		write(root, "img/entry.sh", "#!/bin/sh\necho hi\n")
		write(root, "hack/lib.sh", "# SPDX\nx=1\n")
		code, out, errOut := lint(root)
		Expect(code).To(Equal(0), errOut)
		Expect(out).To(Equal("bash5-lint: 1 scripts, every one starts with the bash 5 guard\n"))
	})
	It("knows the registered hooks when the root has a trailing slash", func() {
		root := GinkgoT().TempDir()
		write(root, "hack/hooks/bash-read-check.sh", "#!/usr/bin/env bash\n"+hookGate+"\n\n"+hookGuard+"\n")
		code, _, errOut := lint(root + "/")
		Expect(code).To(Equal(0), errOut)
	})
	It("fails, names the bad script only, and prints the guard", func() {
		root := GinkgoT().TempDir()
		write(root, "hack/a.sh", "#!/usr/bin/env bash\n"+guard+"\n")
		write(root, "hack/b.sh", "#!/usr/bin/env bash\nset -euo pipefail\n")
		code, _, errOut := lint(root)
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("bash5-lint: hack/b.sh: the first command is not the bash 5 guard\n"))
		Expect(errOut).NotTo(ContainSubstring("hack/a.sh"))
		Expect(errOut).To(ContainSubstring("put this line before the first command of each script listed (ADR 0090):\n" + guard + "\n"))
	})
	It("skips node_modules outside a git checkout", func() {
		root := GinkgoT().TempDir()
		write(root, "node_modules/x/bad.sh", "#!/bin/bash\n")
		code, _, _ := lint(root)
		Expect(code).To(Equal(0))
	})
	It("takes the files from git in a checkout, untracked ones included, ignored ones not", func() {
		testenv.Require(testenv.Binary("git"))
		root := GinkgoT().TempDir()
		// bash5lint starts git itself with the process environment: isolate it for this spec.
		for _, kv := range gitenv.Env(root) {
			k, v, _ := strings.Cut(kv, "=")
			GinkgoT().Setenv(k, v)
		}
		Expect(gitenv.Command(context.Background(), root, root, "init", "-q").Run()).To(Succeed())
		write(root, ".gitignore", "ignored/\n")
		write(root, "g.sh", "#!/bin/bash\n")
		write(root, "ignored/h.sh", "#!/bin/bash\n")
		code, _, errOut := lint(root)
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("bash5-lint: g.sh:"))
		Expect(errOut).NotTo(ContainSubstring("ignored"))
	})
	It("rejects a root that does not exist or is not a directory", func() {
		dir := GinkgoT().TempDir()
		write(dir, "f.sh", "#!/bin/bash\n")
		for _, root := range []string{filepath.Join(dir, "missing"), filepath.Join(dir, "f.sh")} {
			var o, e bytes.Buffer
			_, err := run(root, &o, &e)
			Expect(err).To(MatchError(ContainSubstring("not a directory")))
		}
	})
})
