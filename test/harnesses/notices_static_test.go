// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs read the OpenCode Dockerfile and the checked-in notices. They need no Docker. They
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
				Expect(dockerfile).To(ContainSubstring("/usr/share/doc/koinon-tools/"+name), name)
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
		b, err := os.ReadFile("../../harnesses/opencode/doc/THIRD_PARTY_NOTICES.md")
		Expect(err).NotTo(HaveOccurred())
		for tool, pin := range map[string]string{"ripgrep": "ripgrep/releases/download/", "fd": "sharkdp/fd/releases/download/v", "ast-grep": "ast-grep/releases/download/"} {
			i := strings.Index(dockerfile, pin)
			Expect(i).To(BeNumerically(">=", 0), tool)
			v := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`).FindString(dockerfile[i+len(pin):])
			Expect(v).NotTo(BeEmpty(), tool)
			Expect(string(b)).To(ContainSubstring("## "+tool+" "+v), "notices are for another %s version than the Dockerfile pin", tool)
		}
	})

	It("writes a source offer that names the packages Koinon knows are GPL or LGPL", func() {
		Expect(dockerfile).To(ContainSubstring("GPL-SOURCE-OFFER.txt"))
		Expect(dockerfile).To(ContainSubstring("dpkg-query -W"))
		for _, p := range []string{"bash", "coreutils", "git", "poppler-utils"} {
			Expect(dockerfile).To(MatchRegexp(`for p in [^;]*\b`+p+`\b[^;]*; do`), p)
		}
	})

	It("ships the Bun LGPL notice with the relinking pointers the build greps for", func() {
		for _, s := range []string{"bun/LICENSE.md", "JavaScriptCore", "LGPL", "relink", "oven-sh/webkit"} {
			Expect(dockerfile).To(ContainSubstring(s), s)
		}
	})
})
