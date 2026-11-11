// SPDX-License-Identifier: Apache-2.0

package policycheck_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The evaluator is a test double. Only _test.go files may import it (issue #10), so a production
// binary can never carry it.
var _ = Describe("importers", func() {
	It("are all _test.go files", func() {
		root, err := filepath.Abs("../../..")
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Join(root, "go.mod")).To(BeARegularFile())
		const target = "/internal/testing/policycheck"

		var offenders []string
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			f, perr := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
			if perr != nil {
				return nil // not this spec's business
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if strings.HasSuffix(path, target) || strings.Contains(path, target+"/") {
					rel, _ := filepath.Rel(root, p)
					offenders = append(offenders, rel)
				}
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		// The package's own non-test files do not import itself, so any hit is a production importer.
		Expect(offenders).To(BeEmpty(), "non-test files import the test-only policycheck evaluator")
	})
})
