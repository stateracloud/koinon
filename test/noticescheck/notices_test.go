// SPDX-License-Identifier: Apache-2.0

package noticescheck_test

import (
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// licenceNotices are the files under /usr/share/doc/koinon-tools/ the image carries. The same list
// is in test/harnesses/licences_test.go, which reads them in a built image.
var licenceNotices = []string{
	"fd/LICENSE-MIT", "fd/LICENSE-APACHE",
	"ripgrep/UNLICENSE", "ripgrep/LICENSE-MIT", "ripgrep/COPYING",
	"yq/LICENSE", "ast-grep/LICENSE", "duckdb/LICENSE", "opencode/LICENSE", "bun/LICENSE.md",
	"THIRD_PARTY_NOTICES.md",
	"GPL-SOURCE-OFFER.txt",
}

// These specs read the OpenCode Dockerfile and the checked-in notices. They need no Docker, and
// hack/go-test.sh runs them in the pull request gate (test/harnesses does not run there). They
// fail when a licence notice, the Rust crate notices or the GPL source offer stops being wired
// into the image (#63). What they cannot show is the built image: the build and the
// sandbox-user read in licences_test.go do that.
var _ = Describe("opencode image licence notices (static)", func() {
	var dockerfile string

	BeforeEach(func() {
		b, err := os.ReadFile("../../harnesses/opencode/Dockerfile")
		Expect(err).NotTo(HaveOccurred())
		dockerfile = string(b)
	})

	It("copies every licence notice into /usr/share/doc/koinon-tools", func() {
		for _, name := range licenceNotices {
			if name == "GPL-SOURCE-OFFER.txt" {
				// written by a RUN step, see the next spec
				continue
			}
			dir, file, _ := strings.Cut(name, "/")
			if file == "" {
				// a file directly in the directory, copied under its own name
				Expect(dockerfile).To(MatchRegexp(`(?m)^COPY .* /usr/share/doc/koinon-tools/`+regexp.QuoteMeta(name)+`$`), name)
				continue
			}
			// a tool's file: its directory is copied whole or the file is copied by name
			pat := `(?m)^COPY .*/usr/share/doc/koinon-tools/` + regexp.QuoteMeta(dir) + `/`
			Expect(dockerfile).To(MatchRegexp(pat), "no COPY into koinon-tools/%s", dir)
		}
	})

	It("checks every notice at build time, so a missing one fails the build", func() {
		m := regexp.MustCompile(`(?s)for f in ([^;]+); do \\\n\s*\[ -s "/usr/share/doc/koinon-tools/\$f" \]`).FindStringSubmatch(dockerfile)
		Expect(m).NotTo(BeNil(), "the build-time check loop is missing")
		checked := strings.Fields(strings.ReplaceAll(m[1], "\\\n", " "))
		Expect(checked).To(ConsistOf(licenceNotices))
	})

	It("keeps the Rust crate notices checked in, for every Rust tool of the image", func() {
		b, err := os.ReadFile("../../harnesses/opencode/doc/THIRD_PARTY_NOTICES.md")
		Expect(err).NotTo(HaveOccurred(), "run hack/flavor-rust-notices.sh and commit its output")
		Expect(strings.TrimSpace(string(b))).NotTo(BeEmpty())
		for _, tool := range []string{"ripgrep", "fd", "ast-grep"} {
			Expect(string(b)).To(MatchRegexp(`(?m)^## `+regexp.QuoteMeta(tool)+` \d+\.\d+\.\d+$`), tool)
		}
		Expect(dockerfile).To(MatchRegexp(`(?m)^COPY .*doc/THIRD_PARTY_NOTICES\.md /usr/share/doc/koinon-tools/THIRD_PARTY_NOTICES\.md$`))
	})

	It("names the pinned version of each Rust tool in the notices as the Dockerfile pins it", func() {
		pinned := map[string]string{}
		for tool, pin := range map[string]string{"ripgrep": "ripgrep/releases/download/", "fd": "sharkdp/fd/releases/download/v", "ast-grep": "ast-grep/releases/download/"} {
			pins := regexp.MustCompile(regexp.QuoteMeta(pin)+`([0-9]+\.[0-9]+\.[0-9]+)`).FindAllStringSubmatch(dockerfile, -1)
			Expect(pins).To(HaveLen(2), "%s: one pin per architecture", tool)
			Expect(pins[0][1]).To(Equal(pins[1][1]), "%s: the two architectures pin different versions", tool)
			pinned[tool] = pins[0][1]
		}
		b, err := os.ReadFile("../../harnesses/opencode/doc/THIRD_PARTY_NOTICES.md")
		Expect(err).NotTo(HaveOccurred())
		for tool, v := range pinned {
			Expect(string(b)).To(ContainSubstring("## "+tool+" "+v), "notices are for another %s version than the Dockerfile pin", tool)
		}
	})

	It("writes a source offer built from dpkg-query and checks the four GPL packages in it", func() {
		Expect(dockerfile).To(MatchRegexp(`(?m)^\s*pkgs=\$\(dpkg-query -W -f=`))
		Expect(dockerfile).To(MatchRegexp(`(?m)^\s*\} > /usr/share/doc/koinon-tools/GPL-SOURCE-OFFER\.txt$`))
		for _, p := range []string{"bash", "coreutils", "git", "poppler-utils"} {
			Expect(dockerfile).To(MatchRegexp(`for p in [^;]*\b`+p+`\b[^;]*; do`), p)
		}
	})

	It("checks that the three Rust tools are in the notices at build time", func() {
		Expect(dockerfile).To(MatchRegexp(`(?m)^\s*for t in ripgrep fd ast-grep; do \\$`))
		Expect(dockerfile).To(MatchRegexp(`grep -q "\^## \$t " /usr/share/doc/koinon-tools/THIRD_PARTY_NOTICES\.md`))
	})

	It("checks the OpenCode MIT notice at build time", func() {
		Expect(dockerfile).To(MatchRegexp(`(?m)^\s*grep -q 'MIT License' /usr/share/doc/koinon-tools/opencode/LICENSE; \\$`))
	})

	It("checks the Bun LGPL notice for the relinking pointers at build time", func() {
		for _, s := range []string{"JavaScriptCore", "LGPL", "relink", "oven-sh/webkit"} {
			Expect(dockerfile).To(MatchRegexp(`(?m)^\s*grep -q(i)? '`+regexp.QuoteMeta(s)+`' /usr/share/doc/koinon-tools/bun/LICENSE\.md; \\$`), s)
		}
	})
})
