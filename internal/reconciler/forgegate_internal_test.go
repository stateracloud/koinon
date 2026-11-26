// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A long cause is cut to forgeFailureMax bytes on a rune boundary, so Postgres accepts the text.
func TestForgeFailureCutsOnARuneBoundary(t *testing.T) {
	short := forgeFailure("mint the code-host token: suspended")
	if short != "koinon-reconciler has no forge credential for the agent: mint the code-host token: suspended" {
		t.Fatalf("a short cause changed: %q", short)
	}
	// A three-byte rune that crosses the limit at every offset.
	for pad := range 3 {
		got := forgeFailure(strings.Repeat("x", pad) + strings.Repeat("€", forgeFailureMax))
		if len(got) > forgeFailureMax {
			t.Fatalf("pad %d: %d bytes, more than %d", pad, len(got), forgeFailureMax)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("pad %d: the cut text is not valid UTF-8", pad)
		}
		if len(got) < forgeFailureMax-utf8.UTFMax {
			t.Fatalf("pad %d: %d bytes, cut more than one rune", pad, len(got))
		}
	}
}
