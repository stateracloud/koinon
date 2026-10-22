// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// hermes is the Hermes flavor's contract (flavors/hermes/README.md).
var hermes = flavor{
	name:         "hermes",
	storeFlavor:  store.AgentFlavorHermes,
	defaultImage: "koinon/flavor-hermes:dev",
	dockerfile:   "flavors/hermes",
	writeFile: func(path, content string) fakemodel.ToolCall {
		return fakemodel.ToolCall{Name: "write_file", Arguments: map[string]any{"path": path, "content": content}}
	},
	// Hermes names MCP tools mcp__<server>__<tool>.
	mcpToolName: "mcp__koinon__" + mcpTool,
	// Self-improvement and outside reach are off (flavors/hermes/README.md).
	absentTools: []string{
		"memory", "skills_list", "skill_view", "skill_manage", "session_search",
		"web_search", "web_extract", "browser_navigate", "delegate_task", "execute_code", "cronjob_manage",
	},
	// Hermes probes the model endpoint for local servers (GET /v1/props, /v1/models) without
	// the key: agent/model_metadata.py.
	anonymousProbes: true,
	// Hermes also advertises session/resume; Koinon does not rely on it.
	resume: true,
	// Hermes asks before every write_file and patch (acp_adapter/edit_approval.py), with
	// allow_once and reject_once options only.
	editAsks: true,
	permissionCall: fakemodel.ToolCall{
		Name: "write_file", Arguments: map[string]any{"path": workdir + "/permitted.txt", "content": "koinon\n"},
	},
	allow:  []acp.PermissionOptionKind{acp.AllowOnce},
	reject: []acp.PermissionOptionKind{acp.RejectOnce},
	// Edits arrive with tool kind "edit", a sandbox-write that agentd's default mapping allows.
	agentdVerdict: "allow",
	// Hermes's session/load reports an unknown session as an internal error with
	// data {"details": "not_found"} (acp_adapter/gateway_server.py).
	unknownSession: "not_found",
}
