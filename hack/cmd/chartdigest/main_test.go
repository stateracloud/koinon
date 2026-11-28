// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestChartdigest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "chartdigest")
}

// A fake, low-entropy digest.
const fake = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

const values = `# top comment

harnesses:
  # the default harness
  opencode:
    image:
      repository: sandbox
      tag: ""
      digest: ""   # filled by the release

other:
  digest: ""
`

var _ = Describe("SetDigest", func() {
	It("replaces only the digest scalar and keeps comments, blank lines and layout", func() {
		out, err := SetDigest([]byte(values), fake)
		Expect(err).NotTo(HaveOccurred())
		want := strings.Replace(values, `digest: ""   # filled`, `digest: "`+fake+`"   # filled`, 1)
		Expect(string(out)).To(Equal(want))
	})

	It("handles a plain, a single-quoted and an unset-looking value", func() {
		for _, v := range []string{"abc", "'abc'", `"abc"`, "sha256:old"} {
			in := "harnesses:\n  opencode:\n    image:\n      digest: " + v + "\n"
			out, err := SetDigest([]byte(in), fake)
			Expect(err).NotTo(HaveOccurred(), v)
			Expect(string(out)).To(Equal("harnesses:\n  opencode:\n    image:\n      digest: \"" + fake + "\"\n"))
		}
	})

	It("is idempotent", func() {
		once, err := SetDigest([]byte(values), fake)
		Expect(err).NotTo(HaveOccurred())
		twice, err := SetDigest(once, fake)
		Expect(err).NotTo(HaveOccurred())
		Expect(twice).To(Equal(once))
	})

	It("fails when a key on the path is missing", func() {
		for _, in := range []string{
			"other: 1\n",
			"harnesses:\n  claude: {}\n",
			"harnesses:\n  opencode:\n    image:\n      tag: x\n",
			"",
		} {
			_, err := SetDigest([]byte(in), fake)
			Expect(err).To(HaveOccurred(), in)
		}
	})

	It("fails when the value is not a scalar or is empty without quotes", func() {
		_, err := SetDigest([]byte("harnesses:\n  opencode:\n    image:\n      digest: [a]\n"), fake)
		Expect(err).To(MatchError(ContainSubstring("not a scalar")))
		_, err = SetDigest([]byte("harnesses:\n  opencode:\n    image:\n      digest:\n"), fake)
		Expect(err).To(HaveOccurred())
	})

	It("refuses a digest that is not sha256 and 64 lower-case hex digits", func() {
		for _, d := range []string{
			"", "sha256:", "sha256:abc", "md5:" + strings.Repeat("0", 64),
			"sha256:" + strings.Repeat("0", 63), "sha256:" + strings.Repeat("0", 65),
			"sha256:" + strings.Repeat("G", 64), "sha256:" + strings.Repeat("A", 64),
			" " + fake, fake + "\n", "registry/x@" + fake,
		} {
			_, err := SetDigest([]byte(values), d)
			Expect(err).To(HaveOccurred(), d)
		}
	})

	It("edits the real chart values", func() {
		real, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "koinon", "values.yaml"))
		Expect(err).NotTo(HaveOccurred())
		out, err := SetDigest(real, fake)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Count(string(out), fake)).To(Equal(1))
		Expect(len(out)).To(Equal(len(real) + len(fake)))
	})
})
