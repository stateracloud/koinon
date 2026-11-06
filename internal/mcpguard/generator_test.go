// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"os"
	"os/exec"
	"path/filepath"

	g "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The committed replay corpora are generated output (#70). A change to gen_corpus.py or to the RT1
// fixtures it reads that is not followed by a regeneration would leave the suite replaying traffic
// nobody can reproduce, so this runs the generator into a scratch tree and compares byte for byte.
var _ = g.Describe("Replay corpus generator", func() {
	g.It("reproduces the committed corpora (run python3 -I testdata/replay/gen_corpus.py to refresh)", func() {
		testenv.Require(testenv.Binary("python3"))
		root := g.GinkgoT().TempDir()
		out := filepath.Join(root, "internal/mcpguard/testdata/replay")
		Expect(os.MkdirAll(out, 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(root, "test/fixtures"), 0o755)).To(Succeed())
		rt1, err := filepath.Abs("../../test/fixtures/rt1")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Symlink(rt1, filepath.Join(root, "test/fixtures/rt1"))).To(Succeed())

		cmd := exec.Command("python3", "-I", "testdata/replay/gen_corpus.py", root)
		b, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(b))

		for _, name := range []string{"handbuilt-benign.jsonl", "handbuilt-leaks.jsonl"} {
			want, err := os.ReadFile(filepath.Join("testdata/replay", name))
			Expect(err).NotTo(HaveOccurred())
			got, err := os.ReadFile(filepath.Join(out, name))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got) == string(want)).To(BeTrue(), "%s is stale: run python3 -I internal/mcpguard/testdata/replay/gen_corpus.py and commit", name)
		}
	})
})
