// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLedgerKnowsNoGatewayFields holds ADR 0097 step 1, exit criterion 3: the ledger reads Koinon's
// usage record only. Its source imports no gateway driver and names no gateway's access-log
// attributes; mapping a gateway's log to usage records is the driver's job (package
// internal/gateway/agentgateway). The specs in this directory that run a driver end to end are
// external test files and are not part of what the criterion covers.
func TestLedgerKnowsNoGatewayFields(t *testing.T) {
	forbiddenPrefixes := []string{"koinon.", "gen_ai.", "agw."}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "/internal/gateway/") || strings.Contains(p, "otlp") {
				t.Errorf("%s imports %s: the ledger consumes usage records, not a gateway driver or its log protocol", f, p)
			}
		}
		// Attribute names are string literals: scan the full file for quoted ones.
		for _, pre := range forbiddenPrefixes {
			if strings.Contains(string(src), `"`+pre) {
				t.Errorf("%s names a gateway access-log attribute (%q...): move the mapping into the driver", f, pre)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ledger source files found")
	}
}
