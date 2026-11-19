// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

func TestSplitKeepSeparatesMCPFiles(t *testing.T) {
	keep := map[string]bool{
		"0a1b-aaaaaaaa": true, "mcp-1111-bbbbbbbb": true, "mcpca-1111-cccccccc": true,
	}
	providers, mcp := splitKeep(keep)
	if len(providers) != 1 || !providers["0a1b-aaaaaaaa"] {
		t.Fatalf("provider keeps = %v", providers)
	}
	if len(mcp) != 2 || !mcp["mcp-1111-bbbbbbbb"] || !mcp["mcpca-1111-cccccccc"] {
		t.Fatalf("MCP keeps = %v", mcp)
	}
}

func TestSplitKeyFilesWithoutMCPStoreKeepsOneStore(t *testing.T) {
	r := &Reconciler{}
	parts := r.splitKeyFiles(map[string][]byte{"a": []byte("1"), "mcp-x-1": []byte("2")})
	if len(parts) != 1 || len(parts[0].files) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestIsMCPFile(t *testing.T) {
	for name, want := range map[string]bool{
		"mcp-1-2": true, "mcpv2-1-2-3": true, "mcpca-1-2": true, "0a1b-2": false, "": false,
	} {
		if got := gatewayplan.IsMCPFile(name); got != want {
			t.Errorf("IsMCPFile(%q) = %v, want %v", name, got, want)
		}
	}
}
