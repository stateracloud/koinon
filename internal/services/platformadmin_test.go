// SPDX-License-Identifier: Apache-2.0

package services

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// platformAdminReaders are the functions that may read Principal.PlatformAdmin, with the reason. Every
// other function must call requirePlatformAdmin (issue #1292).
var platformAdminReaders = map[string]string{
	"platformadmin.go:requirePlatformAdmin": "the one platform-admin check",
	"services.go:authorize":                 "org scope: a platform admin holds every role in every org",
	"tokens.go:caller.role":                 "org scope: a platform admin holds the owner role in every org",
}

// platformAdminUses returns "file:func" for each read of a .PlatformAdmin field and for each use of
// the PLATFORM_ADMIN_REQUIRED reason in the non-test Go files of this package.
func platformAdminUses() (reads, reasons []string) {
	files, err := filepath.Glob("*.go")
	Expect(err).NotTo(HaveOccurred())
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		Expect(err).NotTo(HaveOccurred())
		f, err := parser.ParseFile(fset, name, src, 0)
		Expect(err).NotTo(HaveOccurred())
		for _, decl := range f.Decls {
			where := name + ":" + declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "PlatformAdmin":
					reads = append(reads, where)
				case "Reason_PLATFORM_ADMIN_REQUIRED":
					reasons = append(reasons, where)
				}
				return true
			})
		}
	}
	sort.Strings(reads)
	sort.Strings(reasons)
	return reads, reasons
}

// declName is "func" or "Recv.func" for a function declaration, and the empty string for other
// declarations (a package-level var reads the field outside any function).
func declName(decl ast.Decl) string {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

var _ = Describe("the platform-admin check", func() {
	It("is the only code that refuses a caller who is not a platform admin", func() {
		reads, reasons := platformAdminUses()
		for _, r := range reads {
			_, ok := platformAdminReaders[r]
			Expect(ok).To(BeTrue(), "%s reads Principal.PlatformAdmin: call requirePlatformAdmin first instead "+
				"(issue #1292), or add it to platformAdminReaders with a reason if it is org scope", r)
		}
		Expect(reasons).To(ConsistOf("platformadmin.go:requirePlatformAdmin"),
			"only requirePlatformAdmin sets the PLATFORM_ADMIN_REQUIRED reason")
	})

	It("has no stale entry in platformAdminReaders", func() {
		reads, _ := platformAdminUses()
		for r := range platformAdminReaders {
			Expect(reads).To(ContainElement(r), "%s no longer reads Principal.PlatformAdmin: remove it from platformAdminReaders", r)
		}
	})
})
