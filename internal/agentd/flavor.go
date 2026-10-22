// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"fmt"
	"strings"

	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/internal/db/store"
)

// endpoints are the per-agent values a flavor's launch command takes. None is a secret:
// they are visible to everything in the sandbox.
type endpoints struct {
	ModelBaseURL string
	Model        string
	MCPURL       string
}

// entryScripts are the flavors' entry scripts, which start the harness as an ACP agent on its
// stdin and stdout (the flavor contract, ADR 0035, 0048; see flavors/<flavor>/README.md). All
// take the same arguments.
var entryScripts = map[store.AgentFlavor]string{
	store.AgentFlavorOpencode: "/usr/local/bin/koinon-opencode",
	store.AgentFlavorHermes:   "/usr/local/bin/koinon-hermes",
	store.AgentFlavorPi:       "/usr/local/bin/koinon-pi",
}

// launchCommand is the flavor's ACP launch command. Binaries are absolute paths: OpenShell
// does not pass the image's PATH to exec'd processes (ADR 0048).
func launchCommand(f store.AgentFlavor, e endpoints) ([]string, error) {
	script, ok := entryScripts[f]
	if !ok {
		return nil, fmt.Errorf("flavor %q has no ACP launch command", f)
	}
	return []string{
		script,
		"--model-base-url", e.ModelBaseURL,
		"--model", e.Model,
		"--mcp-url", e.MCPURL,
	}, nil
}

// readyCommand is the ACP command a flavor's harness announces once Koinon's MCP tools are
// registered, or "" when it announces none. OpenCode turns the MCP prompt koinon-ready
// (gateway.ReadyPrompt, published by Koinon's MCP server) into the command
// <server>:<prompt>, and its server is "koinon" (flavors/opencode/README.md, ADR 0052).
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

// versionConfig reads an agent version's config with agentconfig.Parse. A config that cannot
// be read counts as empty: the install default model, every harness setting off and no
// instructions of its own.
func versionConfig(raw []byte) (agentconfig.Config, error) {
	c, err := agentconfig.Parse(raw)
	if err != nil {
		return agentconfig.Config{}, err
	}
	return c, nil
}

// versionModel returns the model an agent version names, or "" for the install default.
func versionModel(c agentconfig.Config) string {
	if name := strings.TrimSpace(c.Model); name != "default" {
		return name
	}
	return ""
}

// harnessArgs are the launch command's optional switches from the agent version's harness
// settings ({"harness": {"pluginInstalls": true}}, copied from the template). Only OpenCode
// has one: --plugin-installs lets it load external plugins (flavors/opencode/README.md); the
// reconciler opens the npm registry for the same agents.
func harnessArgs(f store.AgentFlavor, c agentconfig.Config) []string {
	if !c.Harness.PluginInstalls || f != store.AgentFlavorOpencode {
		return nil
	}
	return []string{"--plugin-installs"}
}
