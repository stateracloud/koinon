// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The reconciler copies a v2 credential as ciphertext and never opens it (ADR 0097 decision 12, spec 2). It
// must not hold the gateway's private key, so no non-test source of the reconciler may name the types and
// methods that open a v2 value or load the key.
func TestReconcilerNeverOpensV2(t *testing.T) {
	banned := map[string]bool{
		"RecipientKey": true, "RecipientKeyring": true, "NewRecipientKey": true, "NewRecipientKeyring": true,
		"GenerateRecipientKey": true, "UnwrapRecipientKey": true, "WrapRecipientKey": true, "EnvelopeV2": true,
		"OpenV2": true,
	}
	bannedImports := []string{"/internal/gatewaykeys"}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			for _, b := range bannedImports {
				if strings.Contains(imp.Path.Value, b) {
					t.Errorf("%s imports %s: the reconciler holds no gateway key", path, imp.Path.Value)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && banned[sel.Sel.Name] {
				t.Errorf("%s uses %s: the reconciler never opens a v2 value", path, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
