// SPDX-License-Identifier: Apache-2.0

// Package flavor describes the harness flavors Koinon ships (ADR 0035; v0.1 ships OpenCode only, ADR 0059). Everything that
// differs per flavor lives in one Descriptor here, so adding a flavor means adding one entry
// to the table (plus its image under flavors/, the agent_flavor migration, the schema enum and
// the proto enum, which tests keep in step).
package flavor

import (
	"slices"

	"github.com/stateracloud/koinon/internal/gateway"

	"github.com/stateracloud/koinon/internal/db/store"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
)

// Descriptor is what the Go side needs to know about one flavor.
type Descriptor struct {
	// Name is the flavor's name as stored and as written in templates.
	Name store.AgentFlavor
	// Proto is the flavor's value in the template API.
	Proto templatev1alpha1.Flavor
	// EntryScript is the flavor's entry script, which starts the harness as an ACP agent on its
	// stdin and stdout (the flavor contract, ADR 0035, 0048; flavors/<flavor>/README.md). All
	// take the same arguments.
	EntryScript string
	// ReadyCommand is the ACP command the harness announces once Koinon's MCP tools are
	// registered, or "" when it announces none.
	//
	// OpenCode turns the MCP prompt koinon-ready (gateway.ReadyPrompt, published by Koinon's MCP
	// server) into the command <server>:<prompt>, and its server is "koinon". agentgateway prefixes every
	// prompt and tool with its target (prefixMode always, ADR 0057 amendment), and the prompt comes from the
	// "github" tools target, so the prompt is github_koinon-ready
	// (flavors/opencode/README.md, ADR 0052).
	//
	// A flavor whose harness announces no MCP prompt leaves this empty; it needs no wait only if
	// its first prompt already carries the Koinon tools (the contract suite checks the first chat
	// request of every flavor).
	ReadyCommand string
	// ModelPrefix is put before the model name to form the value agentd selects for the
	// session's model option (session/set_config_option); "" when the flavor takes its model
	// from the launch command alone. OpenCode names models <provider>/<model>; the provider is
	// "koinon".
	ModelPrefix string
	// InstructionLink is the path the flavor's harness reads its instructions from: a symbolic
	// link to the instruction file, which agentd restores before every prompt. "" when the
	// flavor needs none.
	InstructionLink string
	// DefaultBinaries are the network-calling executables of the flavor
	// (flavors/<flavor>/README.md). OpenShell matches the real path of the executable that
	// opens a connection, or of one of its ancestors, never a symbolic link or a script: a
	// script runs as its interpreter, so a harness written as scripts lists its interpreter's
	// real path, which may name the architecture. OpenCode is a single binary.
	DefaultBinaries []string
	// PluginInstalls is set when the flavor supports the template option
	// harness.pluginInstalls: its launch command then takes PluginInstallsArg, and the
	// reconciler opens the npm registry for its binaries.
	PluginInstalls    bool
	PluginInstallsArg string
}

var table = []Descriptor{
	{
		Name:         store.AgentFlavorOpencode,
		Proto:        templatev1alpha1.Flavor_FLAVOR_OPENCODE,
		EntryScript:  "/usr/local/bin/koinon-opencode",
		ReadyCommand: "koinon:" + gateway.ReadyPromptExposed,
		ModelPrefix:  "koinon/",
		// The global config directory's AGENTS.md (flavors/opencode/Dockerfile).
		InstructionLink:   "/etc/koinon/opencode/AGENTS.md",
		DefaultBinaries:   []string{"/usr/local/bin/opencode"},
		PluginInstalls:    true,
		PluginInstallsArg: "--plugin-installs",
	},
}

// All returns copies of the descriptors of every flavor, in a fixed order.
func All() []Descriptor {
	out := make([]Descriptor, len(table))
	for i, d := range table {
		out[i] = d.clone()
	}
	return out
}

// clone returns d with its own copy of the slices, so callers cannot change the table.
func (d Descriptor) clone() Descriptor {
	d.DefaultBinaries = slices.Clone(d.DefaultBinaries)
	return d
}

// Names returns every flavor's name, in the order of All.
func Names() []store.AgentFlavor {
	out := make([]store.AgentFlavor, len(table))
	for i, d := range table {
		out[i] = d.Name
	}
	return out
}

// Lookup returns the descriptor of the flavor named name.
func Lookup(name store.AgentFlavor) (Descriptor, bool) {
	for _, d := range table {
		if d.Name == name {
			return d.clone(), true
		}
	}
	return Descriptor{}, false
}

// FromProto returns the descriptor of the flavor with the API value p.
func FromProto(p templatev1alpha1.Flavor) (Descriptor, bool) {
	for _, d := range table {
		if d.Proto == p {
			return d.clone(), true
		}
	}
	return Descriptor{}, false
}

// SupportsPluginInstalls reports whether the flavor has the harness.pluginInstalls option.
func SupportsPluginInstalls(name store.AgentFlavor) bool {
	d, ok := Lookup(name)
	return ok && d.PluginInstalls
}
