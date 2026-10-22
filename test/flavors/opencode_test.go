// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// opencode is the OpenCode flavor's contract (flavors/opencode/README.md).
var opencode = flavor{
	name:         "opencode",
	storeFlavor:  store.AgentFlavorOpencode,
	defaultImage: "koinon/flavor-opencode:dev",
	dockerfile:   "flavors/opencode",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"filePath": path, "content": content}}
	},
	// OpenCode names MCP tools <server>_<tool>; the server is "koinon" in its config.
	mcpToolName: "koinon_" + mcpTool,
	// OpenCode also advertises session/resume; Koinon does not rely on it.
	resume: true,
	// OpenCode asks before touching a path outside the session's directory
	// (permission external_directory, default "ask"), not for edits inside it.
	permissionCall: fakemodel.ToolCall{
		Name: "read", Arguments: map[string]any{"filePath": "/etc/koinon/opencode/opencode.jsonc"},
	},
	allow:  []acp.PermissionOptionKind{acp.AllowOnce},
	reject: []acp.PermissionOptionKind{acp.RejectOnce},
	// external_directory arrives with tool kind "other", which agentd's default mapping
	// leaves unmapped: rejected.
	agentdVerdict: "reject",
	// OpenCode reports an unknown session as an internal error from its session service.
	unknownSession: "OpenCode service failure",
}
