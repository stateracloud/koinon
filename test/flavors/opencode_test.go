// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// opencode is the OpenCode flavor's contract (flavors/opencode/README.md).
var opencode = flavor{
	name:         "opencode",
	defaultImage: "koinon/flavor-opencode:dev",
	dockerfile:   "flavors/opencode",
	command: func(modelBaseURL, model, mcpURL string) []string {
		return []string{
			"/usr/local/bin/koinon-opencode",
			"--model-base-url", modelBaseURL,
			"--model", model,
			"--mcp-url", mcpURL,
		}
	},
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"filePath": path, "content": content}}
	},
	readFile: func(path string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "read", Arguments: map[string]any{"filePath": path}}
	},
	// OpenCode asks before touching a path outside the session's directory
	// (permission external_directory, default "ask").
	outsidePath: "/etc/koinon/opencode/opencode.jsonc",
	// OpenCode names MCP tools <server>_<tool>; the server is "koinon" in its config.
	mcpToolName: "koinon_" + mcpTool,
	allow:       []acp.PermissionOptionKind{acp.AllowOnce},
	reject:      []acp.PermissionOptionKind{acp.RejectOnce},
}

var _ = describeContract(opencode)
