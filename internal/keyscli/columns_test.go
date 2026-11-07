// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/keyscli"
)

const secretstorePath = "github.com/stateracloud/koinon/internal/secretstore"

// Every secretstore.Binding the code builds names a column that holds envelopes; the key tool
// must know all of them, or rotation could retire a KEK they still need (old issue 96). A plain test,
// not a spec of the suite, so it runs without Docker.
func TestColumnsListEveryBinding(t *testing.T) {
	g := NewWithT(t)
	known := map[keyscli.Column]bool{}
	for _, c := range keyscli.Columns {
		known[c] = true
	}
	found := bindings(g, filepath.Join("..", ".."))
	g.Expect(found).NotTo(BeEmpty(), "the scan found no bindings; it no longer matches the code")
	for where, c := range found {
		g.Expect(known).To(HaveKey(c), "%s binds %s.%s, which keyscli.Columns does not list", where, c.Table, c.Column)
	}
}

// bindings returns the table and column of every secretstore.Binding composite literal in the
// non-test Go files under root, outside secretstore and keyscli (which build bindings from
// Columns), keyed by position. Table and Column must be string constants of the same package.
func bindings(g Gomega, root string) map[string]keyscli.Column {
	out := map[string]keyscli.Column{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		switch d.Name() {
		case ".git", "node_modules", "gen", "secretstore", "keyscli", "testdata":
			return filepath.SkipDir
		}
		files, err := parseDir(fset, dir)
		if err != nil {
			return err
		}
		consts := stringConsts(files)
		for _, f := range files {
			name := importName(g, fset, f)
			if name == "" {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if ok && isBindingType(lit.Type, name) {
					out[fset.Position(lit.Pos()).String()] = bindingColumn(g, fset, lit, consts)
				}
				return true
			})
		}
		return nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	return out
}

// importName returns the name a file uses for the secretstore package, or "" when it does not
// import it. A dot or blank import would hide its bindings from the scan, so it fails.
func importName(g Gomega, fset *token.FileSet, f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != secretstorePath {
			continue
		}
		if imp.Name == nil {
			return "secretstore"
		}
		g.Expect(imp.Name.Name).NotTo(BeElementOf(".", "_"), "%s: import secretstore by name", fset.Position(imp.Pos()))
		return imp.Name.Name
	}
	return ""
}

// parseDir parses the non-test Go files of one directory (one package).
func parseDir(fset *token.FileSet, dir string) ([]*ast.File, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// bindingColumn reads the Table and Column fields of a secretstore.Binding literal.
func bindingColumn(g Gomega, fset *token.FileSet, lit *ast.CompositeLit, consts map[string]string) keyscli.Column {
	var c keyscli.Column
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		g.Expect(ok).To(BeTrue(), "%s: secretstore.Binding needs field names", fset.Position(e.Pos()))
		key, _ := kv.Key.(*ast.Ident)
		if key == nil || (key.Name != "Table" && key.Name != "Column") {
			continue
		}
		v := value(kv.Value, consts)
		g.Expect(v).NotTo(BeEmpty(), "%s: %s must be a string constant", fset.Position(kv.Pos()), key.Name)
		if key.Name == "Table" {
			c.Table = v
		} else {
			c.Column = v
		}
	}
	return c
}

func isBindingType(e ast.Expr, pkgName string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == pkgName && sel.Sel.Name == "Binding"
}

// stringConsts maps the string constants of a directory's files (one package) to their values.
func stringConsts(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							out[name.Name], _ = strconv.Unquote(bl.Value)
						}
					}
				}
			}
		}
	}
	return out
}

func value(e ast.Expr, consts map[string]string) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, _ := strconv.Unquote(v.Value)
		return s
	case *ast.Ident:
		return consts[v.Name]
	}
	return ""
}

// keys forget must know what to do with a row of every column it may find sealed with a lost KEK.
func TestForgetHasARuleForEveryColumn(t *testing.T) {
	g := NewWithT(t)
	for _, c := range keyscli.Columns {
		g.Expect(keyscli.HasLostRule(c)).To(BeTrue(), "keys forget has no rule for %s.%s", c.Table, c.Column)
	}
}
