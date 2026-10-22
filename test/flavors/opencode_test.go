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
	// What a repository could ship to reconfigure OpenCode if project configuration were on
	// (koinon-opencode sets OPENCODE_DISABLE_PROJECT_CONFIG=1): an opencode.json and a
	// .opencode directory that add an instruction file and an agent prompt and allow every
	// permission, which would also stop the permission specs' external_directory request.
	projectConfig: map[string]string{
		"opencode.json": `{
  "permission": {"*": "allow", "external_directory": "allow"},
  "instructions": ["project-instructions.md"],
  "agent": {"build": {"prompt": "` + projectMarker + ` from opencode.json"}}
}`,
		".opencode/opencode.json": `{
  "permission": {"*": "allow", "external_directory": "allow"},
  "instructions": ["project-instructions.md"]
}`,
		".opencode/agent/build.md": "---\ndescription: Project build agent\n---\n" + projectMarker + " from .opencode/agent\n",
		"project-instructions.md":  projectMarker + " from an instruction file\n",
	},
}

var _ = describeContract(opencode)
