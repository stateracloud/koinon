// SPDX-License-Identifier: Apache-2.0

// Package harness describes the harnesses Koinon ships (ADR 0112; OpenCode is the only one, ADR
// 0059). Everything that differs per harness lives in one Descriptor here, so adding a built-in
// harness means adding one entry to the table (plus its image under flavors/, the schema enum and
// the deprecated proto enum, which tests keep in step). ADR 0112 replaces this table with harness
// definitions (#611).
package harness

import (
	"maps"
	"slices"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"

	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
)

// OpenCode is the name of the built-in OpenCode harness.
const OpenCode = "opencode"

// Descriptor is what the Go side needs to know about one harness.
type Descriptor struct {
	// Name is the harness's name as stored and as written in templates.
	Name string
	// Proto is the harness's value in the template API.
	Proto templatev1alpha1.Flavor
	// EntryScript is the harness's entry script, which starts the harness as an ACP agent on its
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
	// A harness whose harness announces no MCP prompt leaves this empty; it needs no wait only if
	// its first prompt already carries the Koinon tools (the contract suite checks the first chat
	// request of every harness).
	ReadyCommand string
	// ModelEntries maps each model format to the harness's provider entry for it (ADR 0069
	// decision 6). agentd selects "<entry>/<model>" as the session's model option
	// (session/set_config_option), and the entry script picks the same entry from its
	// FormatArg. OpenCode names models <provider>/<model>. Empty when the harness takes its model
	// from the launch command alone. A format with no row cannot run on the harness; a new format
	// is one row here, one in the harness's entry script (the case on --model-format in
	// flavors/<flavor>/koinon-<flavor>, with its provider in the configuration) and one in the
	// gateway's table. test/flavors checks that the script and this table agree.
	ModelEntries map[modelformat.Format]string
	// FormatArg is the launch switch that takes the model's format, so the entry script uses the
	// matching entry. "" when the harness has one entry only.
	FormatArg string
	// ThinkingArg is the launch switch that takes the thinking mode Koinon decided for the run
	// (internal/thinking.Mode, the exact form for on or off), so the harness renders it instead of guessing
	// from the model's ID. "" when the harness takes none. The entry script turns it into the
	// harness's own setting (ADR 0069 amendment 2026-10-29).
	ThinkingArg string
	// InstructionLink is the path the harness reads its instructions from: a symbolic
	// link to the instruction file, which agentd restores before every prompt. "" when the
	// harness needs none.
	InstructionLink string
	// DefaultBinaries are the network-calling executables of the harness
	// (flavors/<flavor>/README.md). OpenShell matches the real path of the executable that
	// opens a connection, or of one of its ancestors, never a symbolic link or a script: a
	// script runs as its interpreter, so a harness written as scripts lists its interpreter's
	// real path, which may name the architecture. OpenCode is a single binary.
	DefaultBinaries []string
	// PluginInstalls is set when the harness supports the template option
	// harness.pluginInstalls: its launch command then takes PluginInstallsArg, and the
	// reconciler opens the npm registry for its binaries.
	PluginInstalls    bool
	PluginInstallsArg string
	// CacheArgs maps a prompt-cache marker format (prices.CacheMode.Marker: "anthropic",
	// "alibaba") to the launch switch that makes the harness send those markers (ADR 0068). A
	// format missing here cannot be cached on this harness. A model whose format has its own markers
	// (prices.CacheNative) needs no switch: the harness places them.
	CacheArgs map[string]string
	// NoWebToolsArg is the launch switch that removes the harness's built-in web tools (fetch and
	// search) from the tools offered to the model. koinon-agentd passes it when the agent has no
	// web egress (agentconfig.Config.WebEgress), so the tool set matches the sandbox's network
	// policy (ADR 0080). "" when the harness has no such tools.
	NoWebToolsArg string
	// SessionArg is the launch switch that takes the run ID, which the harness sends to
	// OpenRouter as session_id so a run stays on one upstream and its cache stays warm. "" when
	// the harness cannot.
	SessionArg string
	// CompactionArg is the launch switch that takes the context size, in tokens, at which the
	// harness compacts the conversation (agentd's compactionTokens, ADR 0070 amendment 2026-11-11).
	// "" when the harness takes none.
	CompactionArg string
	// RetriesModelRequests is set when the harness retries a failed model request itself.
	// Exactly one layer retries (ADR 0056 amendment 2026-10-31, point 3), so koinon-agentd then
	// skips its own turn retry for the harness, and gives the harness Koinon's retry policy instead:
	// the entry script takes --model-tries, --model-retry-backoff-ms and --model-max-retry-after-ms
	// (flavors/common/koinon-entry-lib.sh) and makes the harness follow them, not its own defaults.
	// A harness whose harness retries but cannot be given the policy has no place here.
	RetriesModelRequests bool
	// MCPToolPrefix is what the harness puts before the name of a tool it got from Koinon's MCP
	// server in a tool call's title (OpenCode: the server is "koinon", the title "koinon_<tool>").
	// agentd's leak guard feed reports nothing for such a finished call: the gateway saw its
	// result and set the legs from the tool's declaration (ADR 0091 decision 4). "" when the
	// harness's titles cannot be told apart, and every call of kind other then sets both legs.
	MCPToolPrefix string
	// Tools are the command-line tools the harness's image ships for the agent to use, by the
	// name of the command (rg, fd, jq, yq, ast-grep, duckdb, sqlite3, pdftotext, pdfinfo, python3, python, file, git).
	// agentd's "Working with large data" section names a tool only when the harness lists it, so
	// a flavor must list exactly what its image has (flavors/<flavor>/README.md). Empty when the
	// image ships none the agent should rely on.
	Tools []string
	// TruncatedOutputSaved is true when the harness cuts a long tool result and saves the full
	// output to a file it names in the result ("full output saved to FILE"; OpenCode: 2,000 lines
	// or 50 KiB). agentd's instructions then tell the agent to query that file instead of running
	// the command again.
	TruncatedOutputSaved bool
}

var table = []Descriptor{
	{
		Name:         OpenCode,
		Proto:        templatev1alpha1.Flavor_FLAVOR_OPENCODE,
		EntryScript:  "/usr/local/bin/koinon-opencode",
		ReadyCommand: "koinon:" + gateway.ReadyPromptExposed,
		// koinon speaks chat completions (@ai-sdk/openai-compatible), koinon-anthropic the Messages
		// API (@ai-sdk/anthropic); both are in flavors/opencode/etc/koinon/opencode/opencode.jsonc.
		ModelEntries: map[modelformat.Format]string{
			modelformat.OpenAI:    "koinon",
			modelformat.Anthropic: "koinon-anthropic",
		},
		FormatArg: "--model-format",
		// The entry script renders the mode into the model's compatibility flags, so OpenCode's own
		// model-ID heuristic never decides whether the request carries a thinking block.
		ThinkingArg: "--thinking",
		// None: OpenCode 2.2.0 reads the instruction file through the "instructions" key of its
		// read-only config (flavors/opencode/README.md, ADR 0052 amendment 2026-10-28).
		DefaultBinaries:   []string{"/usr/local/bin/opencode"},
		PluginInstalls:    true,
		PluginInstallsArg: "--plugin-installs",
		// The entry script turns each into compatibility.cacheFormat of the koinon provider
		// (OpenCode 2.2.0, packages/schema/src/model-compatibility.ts).
		CacheArgs:     map[string]string{"anthropic": "--cache-anthropic", "alibaba": "--cache-alibaba"},
		NoWebToolsArg: "--no-web-tools",
		SessionArg:    "--session-id",
		CompactionArg: "--compaction-tokens",
		// OpenCode 2.2.0 retries a failed model request up to 10 times on its own schedule
		// (packages/core/src/model-retry.ts). No config key sets it; Koinon's retry plugin, loaded
		// from the read-only config, overrides each decision through the session.retry hook
		// (flavors/opencode/README.md, "Model retries").
		RetriesModelRequests: true,
		MCPToolPrefix:        "koinon_",
		// The tools of the OpenCode image (flavors/opencode/README.md).
		Tools: []string{"rg", "fd", "jq", "yq", "ast-grep", "duckdb", "sqlite3", "pdftotext", "pdfinfo", "python3", "python", "file", "git"},
		// OpenCode saves the full output of a cut tool result to a file.
		TruncatedOutputSaved: true,
	},
}

// All returns copies of the descriptors of every harness, in a fixed order.
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
	d.CacheArgs = maps.Clone(d.CacheArgs)
	d.ModelEntries = maps.Clone(d.ModelEntries)
	d.Tools = slices.Clone(d.Tools)
	return d
}

// Names returns every harness's name, in the order of All.
func Names() []string {
	out := make([]string, len(table))
	for i, d := range table {
		out[i] = d.Name
	}
	return out
}

// Lookup returns the descriptor of the harness named name.
func Lookup(name string) (Descriptor, bool) {
	for _, d := range table {
		if d.Name == name {
			return d.clone(), true
		}
	}
	return Descriptor{}, false
}

// FromProto returns the descriptor of the harness with the API value p.
func FromProto(p templatev1alpha1.Flavor) (Descriptor, bool) {
	for _, d := range table {
		if d.Proto == p {
			return d.clone(), true
		}
	}
	return Descriptor{}, false
}

// SupportsPluginInstalls reports whether the harness has the harness.pluginInstalls option.
func SupportsPluginInstalls(name string) bool {
	d, ok := Lookup(name)
	return ok && d.PluginInstalls
}
