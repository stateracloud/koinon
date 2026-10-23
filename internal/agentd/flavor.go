// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
)

// endpoints are the per-agent values a flavor's launch command takes. None is a secret:
// they are visible to everything in the sandbox.
type endpoints struct {
	ModelBaseURL string
	Model        string
	MCPURL       string
}

// launchCommand is the flavor's ACP launch command. Binaries are absolute paths: OpenShell
// does not pass the image's PATH to exec'd processes (ADR 0048).
func launchCommand(f store.AgentFlavor, e endpoints) ([]string, error) {
	d, ok := flavor.Lookup(f)
	if !ok || d.EntryScript == "" {
		return nil, fmt.Errorf("flavor %q has no ACP launch command", f)
	}
	return []string{
		d.EntryScript,
		"--model-base-url", e.ModelBaseURL,
		"--model", e.Model,
		"--mcp-url", e.MCPURL,
	}, nil
}

// readyCommand is the ACP command a flavor's harness announces once Koinon's MCP tools are
// registered, or "" when it announces none (flavor.Descriptor.ReadyCommand).
func readyCommand(f store.AgentFlavor) string {
	d, _ := flavor.Lookup(f)
	return d.ReadyCommand
}

// modelOption is the value agentd selects for the session's model option
// (session/set_config_option), or "" when the flavor takes its model from the launch command
// alone. OpenCode names models <provider>/<model>; the provider is "koinon".
func modelOption(f store.AgentFlavor, model string) string {
	if d, _ := flavor.Lookup(f); d.ModelPrefix != "" {
		return d.ModelPrefix + model
	}
	return ""
}

// versionConfig reads an agent version's config with agentconfig.Parse. A config that cannot
// be read fails the run (run.go): it would run without its instructions.
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
// settings ({"harness": {"pluginInstalls": true}}, copied from the template). Only flavors
// with the option have one (OpenCode: --plugin-installs lets it load external plugins,
// flavors/opencode/README.md); the reconciler opens the npm registry for the same agents.
func harnessArgs(f store.AgentFlavor, c agentconfig.Config) []string {
	if d, _ := flavor.Lookup(f); c.Harness.PluginInstalls && d.PluginInstalls {
		return []string{d.PluginInstallsArg}
	}
	return nil
}
