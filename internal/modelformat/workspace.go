// SPDX-License-Identifier: Apache-2.0

package modelformat

import "regexp"

// WorkspaceIDPattern and MaxWorkspaceIDLength are the rule for a provider's Anthropic workspace ID
// (ADR 0069 decision 5). Migration 00031's CHECK states the same rule (a test compares them), the
// catalog applies it on save, and the gateway applies it again because the value becomes a header.
const (
	WorkspaceIDPattern   = `^wrkspc_[A-Za-z0-9]+$`
	MaxWorkspaceIDLength = 128
)

var workspaceID = regexp.MustCompile(WorkspaceIDPattern)

// ValidWorkspaceID reports whether id is a well-formed Anthropic workspace ID.
func ValidWorkspaceID(id string) bool {
	return len(id) <= MaxWorkspaceIDLength && workspaceID.MatchString(id)
}
