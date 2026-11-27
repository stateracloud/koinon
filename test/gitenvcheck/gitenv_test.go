// SPDX-License-Identifier: Apache-2.0

package gitenvcheck_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// gitExec finds a test that starts the git binary on the host.
var gitExec = regexp.MustCompile(`exec\.Command(?:Context)?\((?:[\w.()]+,\s*)?"git"`)

// exemptMarker in a test file says the file runs git on purpose with the real environment.
// The marker must carry a reason.
const exemptMarker = "gitenv-exempt:"

// These specs fail when a Go test file starts git without internal/testing/gitenv. A git process
// that inherits os.Environ() reads and writes the real HOME and the global git configuration of the
// person or the runner (#1427). The check is on the file: a file that starts git must use gitenv.
var _ = Describe("git in Go tests", func() {
	It("runs only with the isolated environment of internal/testing/gitenv", func() {
		bad, err := offenders(repoRoot())
		Expect(err).NotTo(HaveOccurred())
		Expect(bad).To(BeEmpty(), "these test files start git without gitenv.Command or gitenv.Env "+
			"(internal/testing/gitenv); or add a comment `// gitenv-exempt: <reason>`")
	})

	It("fails on a test file in the old style (fixture in testdata)", func() {
		bad, err := offenders("testdata")
		Expect(err).NotTo(HaveOccurred())
		Expect(bad).To(ConsistOf(filepath.Join("old", "old_style_test.go")))
	})

	It("finds a test file that starts git with os.Environ()", func() {
		Expect(gitExec.MatchString(`c := exec.Command("git", "init")`)).To(BeTrue())
		Expect(gitExec.MatchString(`exec.CommandContext(ctx, "git", args...)`)).To(BeTrue())
		Expect(gitExec.MatchString(`exec.Command("docker", "git")`)).To(BeFalse())
	})
})

// repoRoot walks up from the test's directory to the directory with the root go.mod.
func repoRoot() string {
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no go.mod above the test")
		dir = parent
	}
}

// offenders returns the test files under root (paths relative to root) that start git without gitenv.
func offenders(root string) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata of the root walk holds the fixture that the check must find.
			if path != root {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "web", "gitenv", "testdata":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		if gitExec.MatchString(src) && !strings.Contains(src, "gitenv.") && !strings.Contains(src, exemptMarker) {
			rel, _ := filepath.Rel(root, path)
			bad = append(bad, rel)
		}
		return nil
	})
	return bad, err
}
