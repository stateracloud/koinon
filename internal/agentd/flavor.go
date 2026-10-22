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
