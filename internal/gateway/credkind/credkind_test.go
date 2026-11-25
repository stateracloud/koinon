// SPDX-License-Identifier: Apache-2.0

package credkind_test

import (
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/project"
)

// The source of truth for the kinds is the constants of type Kind in this package. The spec reads them from the
// source, not from the list, so a new kind without a rule fails here until it gets one (#1221).
var _ = Describe("the credential kind list (#1221)", func() {
	It("gives every declared kind exactly one rule, and every rule a declared kind", func() {
		declared := declaredKinds()
		Expect(declared).NotTo(BeEmpty())
		ruled := map[credkind.Kind]int{}
		for _, r := range credkind.Rules() {
			ruled[r.Kind]++
		}
		for name, k := range declared {
			Expect(ruled).To(HaveKey(k), "credkind.%s (%q) has no rule in the list: give it one, or a NoValue rule "+
				"with the reason it carries no value", name, k)
		}
		values := map[credkind.Kind]bool{}
		for _, k := range declared {
			values[k] = true
		}
		for k, n := range ruled {
			Expect(n).To(Equal(1), "kind %q has %d rules", k, n)
			Expect(values).To(HaveKey(k), "the rule of %q names no declared kind", k)
		}
	})

	It("gives each rule a known class, and a reason to each class that does not store the value", func() {
		for _, r := range credkind.Rules() {
			Expect(r.Scrub).To(BeElementOf(credkind.ByValue, credkind.ByShape, credkind.NoValue), "kind %q", r.Kind)
			if r.Scrub != credkind.ByValue {
				Expect(strings.TrimSpace(r.Why)).NotTo(BeEmpty(), "kind %q is %s and says not why", r.Kind, r.Scrub)
			}
		}
	})

	It("scrubs a kind that is not in the list by value", func() {
		r, ok := credkind.Lookup("koinon_test_unlisted")
		Expect(ok).To(BeFalse())
		Expect(r.Scrub).To(Equal(credkind.ByValue))
		r, ok = credkind.Lookup("")
		Expect(ok).To(BeFalse())
		Expect(r.Scrub).To(Equal(credkind.ByValue))
	})

	It("declares every kind in this package, so the spec above sees it", func() {
		Expect(kindsOutside()).To(BeEmpty(), "declare a credential kind as a constant of package credkind and give it a rule")
	})
})

var _ = Describe("the passport shape (ADR 0073)", func() {
	It("matches a JWT of the passport typ, alone or after Bearer", func() {
		p := fakeJWT(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`)
		Expect(credkind.IsPassport(p)).To(BeTrue())
		Expect(credkind.IsPassport("Bearer " + p)).To(BeTrue())
		Expect(credkind.IsPassport(fakeJWT(`{"typ":"application/KOINON-PASSPORT+JWT","alg":"EdDSA"}`))).To(BeTrue())
		Expect(credkind.IsPassport(p + " and more")).To(BeFalse())
	})

	It("does not match a JWT of another typ, or a text that only looks like one", func() {
		Expect(credkind.IsPassport(fakeJWT(`{"alg":"RS256","typ":"JWT"}`))).To(BeFalse())
		Expect(credkind.IsPassport(fakeJWT(`{"alg":"RS256"}`))).To(BeFalse())
		Expect(credkind.IsPassport("eyJub3Rqc29u.eyJ4IjoxfQ.sig")).To(BeFalse())
		Expect(credkind.FindPassports("the key and they and eyes")).To(BeEmpty())
	})

	It("finds each passport in a text and leaves other JWTs", func() {
		p := fakeJWT(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`)
		other := fakeJWT(`{"alg":"RS256","typ":"JWT"}`)
		text := `{"echo":"Bearer ` + p + `","id_token":"` + other + `"}` + p
		found := credkind.FindPassports(text)
		Expect(found).To(HaveLen(2))
		for _, f := range found {
			Expect(text[f[0]:f[1]]).To(Equal(p))
		}
	})
})

// fakeJWT returns a compact JWS with the given header, a fixed fake payload and a fake signature.
func fakeJWT(header string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(header) + "." + enc(`{"sub":"koinon_test_not-a-real-agent"}`) + ".koinon_test_not-a-real-signature"
}

// declaredKinds parses this package and returns each constant of type Kind by name.
func declaredKinds() map[string]credkind.Kind {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	Expect(err).NotTo(HaveOccurred())
	out := map[string]credkind.Kind{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Kind" {
					continue
				}
				for i, n := range vs.Names {
					Expect(i).To(BeNumerically("<", len(vs.Values)), "kind %s has no value", n.Name)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					Expect(ok).To(BeTrue(), "kind %s is not a string literal", n.Name)
					v, err := strconv.Unquote(lit.Value)
					Expect(err).NotTo(HaveOccurred())
					out[n.Name] = credkind.Kind(v)
				}
			}
		}
	}
	return out
}

// kindsOutside walks the module and returns each place outside this package that makes a new Kind value: a
// constant or variable of type credkind.Kind whose value is not a credkind constant, or a conversion of a
// literal to credkind.Kind. Test files are not read: specs make unlisted kinds on purpose.
func kindsOutside() []string {
	root := moduleRoot()
	self, err := filepath.Abs(".")
	Expect(err).NotTo(HaveOccurred())
	importPath := project.ModulePath + "/internal/gateway/credkind"
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", "web":
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), importPath) {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		name := "credkind"
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == importPath && im.Name != nil {
				name = im.Name.Name
			}
		}
		isKind := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Kind" {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == name
		}
		fromPkg := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == name
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				if n.Type != nil && isKind(n.Type) {
					for _, v := range n.Values {
						if !fromPkg(v) {
							bad = append(bad, fset.Position(v.Pos()).String())
						}
					}
				}
			case *ast.CallExpr:
				if isKind(n.Fun) && len(n.Args) == 1 {
					if _, lit := n.Args[0].(*ast.BasicLit); lit {
						bad = append(bad, fset.Position(n.Pos()).String())
					}
				}
			}
			return true
		})
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
	return bad
}

func moduleRoot() string {
	dir, err := filepath.Abs(".")
	Expect(err).NotTo(HaveOccurred())
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no go.mod above the package")
		dir = parent
	}
}
