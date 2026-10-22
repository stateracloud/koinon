// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// pi is the pi flavor's contract (flavors/pi/README.md).
var pi = flavor{
	name:         "pi",
	storeFlavor:  store.AgentFlavorPi,
	defaultImage: "koinon/flavor-pi:dev",
	dockerfile:   "flavors/pi",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write", Arguments: map[string]any{"path": path, "content": content}}
	},
	// pi names MCP tools mcp__<server>__<tool>; "exposure": "direct" declares them to the model.
	mcpToolName: "mcp__koinon__" + mcpTool,
	// pi-acp advertises session/load, list and delete, not resume.
	resume: false,
	// pi-acp asks over ACP only for pi extension dialogs; built-in tools never ask.
	editAsks:       false,
	unknownSession: "Unknown sessionId",
}
