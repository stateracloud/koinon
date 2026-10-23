// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/procoduck/koinon/internal/db/store"
)

// endpoints are the per-agent values a flavor's launch command takes. None is a secret:
// they are visible to everything in the sandbox.
type endpoints struct {
	ModelBaseURL string
	Model        string
	MCPURL       string
}

// launchCommand is the flavor's ACP launch command (the flavor contract, ADR 0035; for
// OpenCode see flavors/opencode/README.md). Binaries are absolute paths: OpenShell does not
// pass the image's PATH to exec'd processes (ADR 0048).
func launchCommand(f store.AgentFlavor, e endpoints) ([]string, error) {
	switch f {
	case store.AgentFlavorOpencode:
		return []string{
			"/usr/local/bin/koinon-opencode",
			"--model-base-url", e.ModelBaseURL,
			"--model", e.Model,
			"--mcp-url", e.MCPURL,
		}, nil
	default:
		// Hermes and pi arrive with plan items F2 and F3.
		return nil, fmt.Errorf("flavor %q has no ACP launch command yet", f)
	}
}

// readyCommand is the ACP command a flavor's harness announces once Koinon's MCP tools are
// registered, or "" when it announces none. OpenCode turns the MCP prompt koinon-ready
// (gateway.ReadyPrompt, published by Koinon's MCP server) into the command
// <server>:<prompt>, and its server is "koinon" (flavors/opencode/README.md, ADR 0052).
//
// Hermes and pi announce no MCP prompt, and need no wait: their first prompt already carries
// the Koinon tools (the contract suite checks the first chat request of every flavor).
//   - Hermes 0.22.3 announces only its built-in commands (acp_adapter/commands.py:74). Its
//     gateway daemon connects the MCP servers before it starts serving
//     (gateway/run_bootstrap.py:679, awaited before runner.start at :684), and `hermes acp`
//     waits for the daemon to be ready before session/new (hermes_cli/gateway_client.py:288).
//   - pi 1.3.0 connects MCP servers in the background on session_start
//     (dist/extensions/mcp/index.js:971), and the first prompt waits up to 10 s for servers
//     with "exposure": "direct", Koinon's (index.js:1014-1036, DEFAULT_STARTUP_WAIT_MS at :39),
//     longer than any fixed delay agentd would add. pi-acp leaves extension commands, MCP
//     prompts among them, out of available_commands_update (pi-acp dist/index.js:2102).
func readyCommand(f store.AgentFlavor) string {
	if f == store.AgentFlavorOpencode {
		return "koinon:koinon-ready"
	}
	return ""
}

// modelOption is the value agentd selects for the session's model option
// (session/set_config_option), or "" when the flavor takes its model from the launch command
// alone. OpenCode names models <provider>/<model>; the provider is "koinon".
func modelOption(f store.AgentFlavor, model string) string {
	if f == store.AgentFlavorOpencode {
		return "koinon/" + model
	}
	return ""
}

// versionModel returns the model an agent version names, or "" for the install default.
// The version config holds it as {"model": {"name": "..."}}, as templates do, or as a
// string.
func versionModel(config []byte) string {
	var c struct {
		Model json.RawMessage `json:"model"`
	}
	if json.Unmarshal(config, &c) != nil || len(c.Model) == 0 {
		return ""
	}
	var name string
	if json.Unmarshal(c.Model, &name) != nil {
		var m struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(c.Model, &m) != nil {
			return ""
		}
		name = m.Name
	}
	if name = strings.TrimSpace(name); name == "default" {
		return ""
	}
	return name
}

// harnessArgs are the launch command's optional switches from the agent version's harness
// settings ({"harness": {"pluginInstalls": true}}, copied from the template). Only OpenCode
// has one: --plugin-installs lets it load external plugins (flavors/opencode/README.md); the
// reconciler opens the npm registry for the same agents. A config that cannot be read turns
// every setting off.
func harnessArgs(f store.AgentFlavor, config []byte) []string {
	var c struct {
		Harness struct {
			PluginInstalls bool `json:"pluginInstalls"`
		} `json:"harness"`
	}
	if json.Unmarshal(config, &c) != nil || !c.Harness.PluginInstalls || f != store.AgentFlavorOpencode {
		return nil
	}
	return []string{"--plugin-installs"}
}
