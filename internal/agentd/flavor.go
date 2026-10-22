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
