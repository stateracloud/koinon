// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/codehost"
)

// githubCaps are the capabilities of the embedded GitHub connector type, from its data.
func githubCaps() codehost.Capabilities {
	t, ok := connectors.Get("github")
	Expect(ok).To(BeTrue())
	caps, err := codehost.CapabilitiesOf(t)
	Expect(err).NotTo(HaveOccurred())
	return caps
}

// giteaCaps are the CI paths ADR 0062 lists for Gitea (the Gitea connector is plan item CH5).
func giteaCaps() codehost.Capabilities {
	return codehost.Capabilities{CIPaths: []string{".gitea/workflows/", ".gitea/scoped_workflows/", ".github/workflows/"}}
}

func write(p string) codehost.FileWrite {
	return codehost.FileWrite{Path: p, Mode: "100644", Content: []byte("x")}
}

func link(p, target string) codehost.FileWrite {
	return codehost.FileWrite{Path: p, Mode: "120000", Content: []byte(target)}
}

var _ = Describe("the workflow guard", func() {
	guard := func(caps codehost.Capabilities, dirs ...string) codehost.Guard {
		g, err := codehost.NewGuard(caps, dirs)
		Expect(err).NotTo(HaveOccurred())
		return g
	}

	DescribeTable("refuses",
		func(caps func() codehost.Capabilities, dirs []string, p string) {
			Expect(guard(caps(), dirs...).Refuse(p)).NotTo(BeEmpty(), p)
			Expect(guard(caps(), dirs...).CheckFiles([]codehost.FileWrite{write(p)})).To(HaveOccurred(), p)
		},
		Entry("a GitHub workflow", githubCaps, nil, ".github/workflows/ci.yml"),
		Entry("a nested GitHub workflow", githubCaps, nil, ".github/workflows/sub/ci.yml"),
		Entry("a GitHub workflow in another case (GitHub folds case)", githubCaps, nil, ".GitHub/Workflows/ci.yml"),
		Entry("a GitHub workflow behind ./ and ..", githubCaps, nil, "./src/../.github/workflows/ci.yml"),
		Entry("the guarded directory written as a file", githubCaps, nil, ".github/workflows"),
		Entry("a Gitea workflow", giteaCaps, nil, ".gitea/workflows/build.yaml"),
		Entry("a Gitea scoped workflow", giteaCaps, nil, ".gitea/scoped_workflows/x.yml"),
		Entry("a workflow Gitea reads from .github", giteaCaps, nil, ".github/workflows/ci.yml"),
		Entry("a directory configured on the connection", giteaCaps, []string{"ci/pipelines"}, "ci/pipelines/a.yml"),
		Entry("a configured directory written with ./ and a trailing slash", giteaCaps, []string{"./ci/pipelines/"}, "ci/pipelines/deep/a.yml"),
		Entry("a configured directory on GitHub", githubCaps, []string{"build/ci"}, "build/ci/x.yml"),
		Entry("a path with backslashes", giteaCaps, nil, `.gitea\workflows\x.yml`),
	)

	DescribeTable("allows",
		func(caps func() codehost.Capabilities, dirs []string, p string) {
			Expect(guard(caps(), dirs...).Refuse(p)).To(BeEmpty(), p)
		},
		Entry("a sibling that only shares a prefix", githubCaps, nil, ".github/workflows-docs/readme.md"),
		Entry("other .github files", githubCaps, nil, ".github/CODEOWNERS"),
		Entry("a Gitea workflow directory on GitHub", githubCaps, nil, ".gitea/workflows/x.yml"),
		Entry("another case on a provider that matches exactly (Gitea)", giteaCaps, nil, ".Gitea/Workflows/x.yml"),
		Entry("an unconfigured directory", giteaCaps, []string{"ci/pipelines"}, "ci/other/x.yml"),
	)

	It("matches case only when the provider says so", func() {
		Expect(githubCaps().CIPathsFoldCase).To(BeTrue())
		Expect(giteaCaps().CIPathsFoldCase).To(BeFalse())
		Expect(guard(giteaCaps(), "CI/Pipelines").Refuse("ci/pipelines/a.yml")).To(BeEmpty())
		Expect(guard(giteaCaps(), "CI/Pipelines").Refuse("CI/Pipelines/a.yml")).NotTo(BeEmpty())
	})

	It("refuses a move into or out of a guarded directory: a delete and a write are both checked", func() {
		g := guard(githubCaps())
		Expect(g.CheckFiles([]codehost.FileWrite{{Path: "src/ci.yml", Delete: true}, write(".github/workflows/ci.yml")})).To(HaveOccurred())
		Expect(g.CheckFiles([]codehost.FileWrite{{Path: ".github/workflows/ci.yml", Delete: true}, write("src/ci.yml")})).To(HaveOccurred())
		Expect(g.CheckFiles([]codehost.FileWrite{{Path: "src/a.yml", Delete: true}, write("src/b.yml")})).To(Succeed())
	})

	DescribeTable("refuses a symbolic link that reaches a guarded path",
		func(l codehost.FileWrite) {
			g := guard(githubCaps(), "ci/pipelines")
			Expect(g.CheckFiles([]codehost.FileWrite{l})).To(HaveOccurred(), string(l.Content))
		},
		Entry("into the directory", link("wf", ".github/workflows")),
		Entry("into a file in it", link("wf.yml", ".github/workflows/ci.yml")),
		Entry("relative to the link", link("a/b/wf", "../../.github/workflows")),
		Entry("at a parent of the directory", link("gh", ".github")),
		Entry("at the repository root", link("root", ".")),
		Entry("into a configured directory", link("p", "ci/pipelines")),
		Entry("absolute", link("abs", "/etc/passwd")),
		Entry("out of the repository", link("out", "../../x")),
		Entry("inside the guarded directory itself", link(".github/workflows/l", "x")),
	)

	It("allows a symbolic link elsewhere", func() {
		Expect(guard(githubCaps()).CheckFiles([]codehost.FileWrite{link("docs/latest", "v2"), link("a/b", "../c/d")})).To(Succeed())
	})

	It("validates configured directories", func() {
		for _, bad := range []string{"", "/abs", "../up", "a/../../b", ".", "a\\b", "a\x00b"} {
			_, err := codehost.NewGuard(giteaCaps(), []string{bad})
			Expect(err).To(HaveOccurred(), bad)
		}
		got, err := codehost.CleanCIDirs([]string{"./a//b/", "a/b", " c ", ""})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]string{"a/b", "c"}))
		_, err = codehost.CleanCIDirs(make([]string, codehost.MaxCIDirs+1))
		Expect(err).To(HaveOccurred())
	})

	It("reads the directories from a connection's config", func() {
		g, err := codehost.GuardOf(giteaCaps(), []byte(`{"baseUrl":"https://git.example.test","ciDirs":["ci"]}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Paths()).To(Equal([]string{".gitea/workflows/", ".gitea/scoped_workflows/", ".github/workflows/", "ci"}))
		Expect(g.Refuse("ci/x")).To(Equal("ci"))
		g, err = codehost.GuardOf(giteaCaps(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Paths()).To(HaveLen(3))
	})

	It("refuses nothing with the zero guard", func() {
		Expect(codehost.Guard{}.CheckFiles([]codehost.FileWrite{write(".github/workflows/x.yml")})).To(Succeed())
	})
})
