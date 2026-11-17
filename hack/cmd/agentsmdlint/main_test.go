// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentsmdlint(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentsmdlint")
}

func write(root, rel, content string) {
	p := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
	Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
}

func lintDir(root string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code, err := run(root, &o, &e)
	Expect(err).NotTo(HaveOccurred())
	return code, o.String(), e.String()
}

const nestedOK = "The root AGENTS.md applies too; this file adds rules for `charts/`.\n\n- Render charts with the chart check before you push anything.\n"

// tree writes a clean tree: a root with one index line, one nested file, a Makefile and a script.
func tree() string {
	root := GinkgoT().TempDir()
	write(root, "AGENTS.md", "# AGENTS.md\n\n- Build: `make build`; run `hack/x.sh` before you push.\n- Editing under `charts/`: read `charts/AGENTS.md`.\n")
	write(root, "charts/AGENTS.md", nestedOK)
	write(root, "Makefile", "build: ## Build\n\ttrue\n")
	write(root, "hack/x.sh", "#!/bin/sh\n")
	return root
}

var _ = Describe("agentsmdlint", func() {
	It("accepts a clean tree", func() {
		code, out, errOut := lintDir(tree())
		Expect(errOut).To(BeEmpty())
		Expect(code).To(Equal(0))
		Expect(out).To(ContainSubstring("agents-md ok"))
	})

	It("rejects a CLAUDE.md anywhere, whatever its case", func() {
		root := tree()
		write(root, "charts/claude.md", "@AGENTS.md\n")
		code, _, errOut := lintDir(root)
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("charts/claude.md: no CLAUDE.md in the tree"))
	})

	It("finds the root file when a dot folder sorts before it", func() {
		root := tree()
		write(root, ".github/AGENTS.md", firstLine(".github")+"\n")
		write(root, "AGENTS.md", "- Editing under `charts/`: read `charts/AGENTS.md`.\n- Editing under `.github/`: read `.github/AGENTS.md`.\n")
		code, _, errOut := lintDir(root)
		Expect(errOut).To(BeEmpty())
		Expect(code).To(Equal(0))
	})

	It("fails without a root file", func() {
		root := GinkgoT().TempDir()
		write(root, "charts/AGENTS.md", nestedOK)
		code, _, errOut := lintDir(root)
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("the root file is missing"))
	})

	It("reports a usage error for a root that does not exist", func() {
		var o, e bytes.Buffer
		code, err := run(filepath.Join(GinkgoT().TempDir(), "gone"), &o, &e)
		Expect(err).To(HaveOccurred())
		Expect(code).To(Equal(2))
	})

	Describe("the index", func() {
		It("wants a line for every nested file", func() {
			root := tree()
			write(root, "internal/db/AGENTS.md", firstLine("internal/db")+"\n")
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("the index has no line for `internal/db/AGENTS.md`"))
		})

		It("refuses a line for a file that does not exist", func() {
			root := tree()
			write(root, "AGENTS.md", "- Editing under `charts/`: read `charts/AGENTS.md`.\n- Editing under `web/`: read `web/AGENTS.md`.\n")
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("names `web/AGENTS.md`, which does not exist"))
		})
	})

	Describe("budgets", func() {
		It("fails a root above 150 lines and notes one above 110", func() {
			root := tree()
			body := "- Editing under `charts/`: read `charts/AGENTS.md`.\n" + strings.Repeat("\n", 120)
			write(root, "AGENTS.md", body)
			code, out, _ := lintDir(root)
			Expect(code).To(Equal(0))
			Expect(out).To(ContainSubstring("note: AGENTS.md has 121 lines; the target is 110"))
			write(root, "AGENTS.md", body+strings.Repeat("\n", 40))
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("AGENTS.md: 161 lines; the ceiling is 150"))
		})

		It("fails a nested file above 120 lines", func() {
			root := tree()
			write(root, "charts/AGENTS.md", nestedOK+strings.Repeat("\n", 125))
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("charts/AGENTS.md: 128 lines; the ceiling is 120"))
		})

		It("fails a file above 10 KB", func() {
			root := tree()
			write(root, "charts/AGENTS.md", nestedOK+strings.Repeat(strings.Repeat("x", 99)+"\n", 105))
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("no file may be above 10240"))
		})
	})

	Describe("the first line of a nested file", func() {
		It("must name the root and its own folder", func() {
			root := tree()
			write(root, "charts/AGENTS.md", "# Charts\n\n- Render charts with the chart check before you push anything.\n")
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("the first line must be: The root AGENTS.md applies too; this file adds rules for `charts/`."))
		})

		It("refuses another folder's name", func() {
			root := tree()
			write(root, "charts/AGENTS.md", firstLine("web")+"\n")
			code, _, _ := lintDir(root)
			Expect(code).To(Equal(1))
		})
	})

	Describe("references", func() {
		It("rejects a make target the Makefile lacks", func() {
			root := tree()
			write(root, "AGENTS.md", "- Run `make nope`.\n- Editing under `charts/`: read `charts/AGENTS.md`.\n")
			_, _, errOut := lintDir(root)
			Expect(errOut).To(ContainSubstring("`make nope`: the Makefile has no such target"))
		})

		It("rejects a hack script or repo path that is gone", func() {
			root := tree()
			write(root, "charts/AGENTS.md", nestedOK+"- Run `hack/gone.sh --flag` and read `charts/missing/values.yaml`.\n")
			_, _, errOut := lintDir(root)
			Expect(errOut).To(ContainSubstring("`hack/gone.sh`: the path does not exist"))
			Expect(errOut).To(ContainSubstring("`charts/missing/values.yaml`: the path does not exist"))
		})

		It("ignores placeholders, globs, absolute paths, env values and fenced blocks", func() {
			root := tree()
			write(root, "charts/AGENTS.md", nestedOK+
				"- Name a file `hack/chart-check.d/NN-<feature>.sh` or `charts/*/values.yaml` under `/sandbox/work`; set `KUBECONFIG=hack/none`.\n"+
				"```\nmake nope hack/gone.sh\n```\n")
			code, _, errOut := lintDir(root)
			Expect(errOut).To(BeEmpty())
			Expect(code).To(Equal(0))
		})

		It("checks relative links and ignores web links and anchors", func() {
			root := tree()
			write(root, "charts/AGENTS.md", nestedOK+"- Read [ci](../docs/ci.md#x), [web](https://example.com/a) and [here](#top).\n")
			_, _, errOut := lintDir(root)
			Expect(errOut).To(ContainSubstring("link ../docs/ci.md#x: the file does not exist"))
			Expect(errOut).NotTo(ContainSubstring("example.com"))
			write(root, "docs/ci.md", "x\n")
			code, _, _ := lintDir(root)
			Expect(code).To(Equal(0))
		})
	})

	Describe("duplicate rules", func() {
		It("rejects a sentence that two files share, however it is wrapped or marked up", func() {
			root := tree()
			write(root, "AGENTS.md", "- **Never** hard-code the module path in Go code.\n- Editing under `charts/`: read `charts/AGENTS.md`.\n")
			write(root, "charts/AGENTS.md", nestedOK+"- Never hard-code\n  the module path in `Go` code. Other text.\n")
			code, _, errOut := lintDir(root)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("charts/AGENTS.md: rule also in AGENTS.md: never hard-code the module path in go code"))
		})

		It("ignores short sentences and a repeat inside one file", func() {
			root := tree()
			write(root, "AGENTS.md", "- Use make.\n- Use make.\n- Editing under `charts/`: read `charts/AGENTS.md`.\n")
			write(root, "charts/AGENTS.md", nestedOK+"- Use make.\n")
			code, _, errOut := lintDir(root)
			Expect(errOut).To(BeEmpty())
			Expect(code).To(Equal(0))
		})
	})
})
