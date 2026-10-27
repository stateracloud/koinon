// SPDX-License-Identifier: Apache-2.0

package githubtools

import "testing"

func TestScopeableDecidesPerRiskType(t *testing.T) {
	if !scopeable(struct{}{}) || !scopeable(Risk{}) {
		t.Fatal("empty and clean risks are covered")
	}
	if scopeable(&Risk{}) || scopeable(map[string]any{}) || scopeable(nil) {
		t.Fatal("pointer and unknown risk types are never covered")
	}
}
