// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/execution"
	harnessdesc "github.com/stateracloud/koinon/internal/harness"
)

// Plugin installs (template option harness.pluginInstalls, off by default). An OpenCode agent
// whose version turns it on gets the sandbox policy entry npm_registry: read-only access to
// the public npm registry, enforced at L7, for the harness's own binaries (OpenCode installs
// plugins in-process, flavors/opencode/README.md). No credential is attached: the registry
// is read anonymously. With the option off nothing is rendered, so the sandbox cannot reach
// the registry at all.
const npmRule = "npm_registry"

// NPMRegistryHost is the registry plugin installs read from.
const NPMRegistryHost = "registry.npmjs.org"

// PluginInstallRule returns the npm_registry policy entry for an agent of harness whose
// version is cfg, running the binaries of f. ok is false when the version does not turn
// plugin installs on or the harness lacks the option (harness.Descriptor.PluginInstalls).
func PluginInstallRule(cfg agentconfig.Config, name string, f Harness) (rule execution.NetworkRule, ok bool) {
	if !cfg.Harness.PluginInstalls || !harnessdesc.SupportsPluginInstalls(name) || len(f.Binaries) == 0 {
		return execution.NetworkRule{}, false
	}
	return execution.NetworkRule{
		Endpoints: []execution.Endpoint{{
			Host: NPMRegistryHost, Port: 443, Protocol: "rest",
			Allow: []execution.L7Rule{{Method: "GET", Path: "/**"}, {Method: "HEAD", Path: "/**"}},
		}},
		Binaries: slices.Clone(f.Binaries),
	}, true
}
