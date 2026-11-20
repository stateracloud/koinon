// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs read the Dockerfile and the release script. They need no Docker: the built image's
// labels are the Dockerfile's LABEL lines plus the release script's --label flags (#1182).
var _ = Describe("harness image base and labels", func() {
	var dockerfile string

	BeforeEach(func() {
		b, err := os.ReadFile("../../harnesses/opencode/Dockerfile")
		Expect(err).NotTo(HaveOccurred())
		dockerfile = string(b)
	})

	It("builds on plain Ubuntu pinned by digest, never on the NVIDIA registry", func() {
		m := regexp.MustCompile(`(?m)^ARG BASE=(\S+)$`).FindStringSubmatch(dockerfile)
		Expect(m).NotTo(BeNil(), "the Dockerfile has no ARG BASE= default")
		Expect(m[1]).To(MatchRegexp(`^docker\.io/library/ubuntu:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`))
		Expect(dockerfile).NotTo(ContainSubstring("nvcr.io"))
		Expect(dockerfile).NotTo(ContainSubstring("nvidia"))
	})

	It("sets its own title, vendor, description and licenses labels", func() {
		for _, key := range []string{"title", "vendor", "description", "licenses"} {
			Expect(dockerfile).To(MatchRegexp(`org\.opencontainers\.image\.`+key+`="[^"]+"`), key)
		}
		Expect(dockerfile).NotTo(ContainSubstring("NVIDIA"))
		Expect(dockerfile).To(MatchRegexp(`image\.licenses="LicenseRef-koinon-image-notices"`))
	})

	It("sets the labels in the final stage, after the base image's own labels", func() {
		// The base sets org.opencontainers.image.version=24.04 itself. A LABEL line after the last
		// FROM is applied on top of it, so our value is the one that is left.
		lastFrom := strings.LastIndex(dockerfile, "\nFROM ")
		label := strings.Index(dockerfile, "\nLABEL org.opencontainers.image.title=")
		Expect(lastFrom).To(BeNumerically(">=", 0))
		Expect(label).To(BeNumerically(">", lastFrom), "the LABEL line must come after the final FROM")
		Expect(dockerfile[label:]).To(ContainSubstring(`org.opencontainers.image.version="dev"`))
		Expect(strings.Count(dockerfile, "image.version=")).To(Equal(1))
	})

	It("keeps the release script's licenses label in step and no Apache-2.0 only claim", func() {
		b, err := os.ReadFile("../../hack/release-harness.sh")
		Expect(err).NotTo(HaveOccurred())
		script := string(b)
		Expect(script).To(ContainSubstring("--label org.opencontainers.image.licenses=LicenseRef-koinon-image-notices"))
		Expect(strings.Contains(script, "image.licenses=Apache-2.0")).To(BeFalse())
	})
})
