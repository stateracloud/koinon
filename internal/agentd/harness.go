// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/thinking"
)

// endpoints are the per-agent values a harness's launch command takes. None is a secret:
// they are visible to everything in the sandbox.
type endpoints struct {
	ModelBaseURL string
	Model        string
	MCPURL       string
	// Format is the model's wire format (models.format). The harness's entry script uses the
	// provider entry for it (harness.Descriptor.ModelEntries). Empty leaves the switch out, which
	// the entry script reads as chat completions.
	Format modelformat.Format
	// CacheMarker is the cache-marker format the model's provider needs (prices.CacheMode.Marker),
	// or "" when it needs none.
	CacheMarker string
	// Thinking is the thinking form Koinon decided for the run (thinking.Capability.Effective): none,
	// or the model's on or off form. Empty leaves the switch out. The harness renders it; its own guess from
	// the model ID never decides (ADR 0069 amendment 2026-10-29).
	Thinking thinking.Mode
	// SessionID is the run ID to send to OpenRouter as session_id (sticky routing), or "".
	SessionID string
	// Retry is Koinon's model-retry policy, handed to a harness whose harness retries model
	// requests itself (harness.Descriptor.RetriesModelRequests); other harnesses ignore it.
	Retry retryPolicy
}

// retryPolicy is Koinon's policy for retrying a failed model request (Config.ModelRetries,
// RetryBackoff, MaxRetryAfter), as the layer that applies it takes it.
type retryPolicy struct {
	// Tries counts the first request: retries plus one.
	Tries         int
	Backoff       time.Duration
	MaxRetryAfter time.Duration
}

// retryPolicyOf is the policy in cfg.
func retryPolicyOf(cfg Config) retryPolicy {
	return retryPolicy{Tries: cfg.ModelRetries + 1, Backoff: cfg.RetryBackoff, MaxRetryAfter: cfg.MaxRetryAfter}
}

// args are the entry-script switches that carry the policy (flavors/common/koinon-entry-lib.sh).
func (p retryPolicy) args() []string {
	return []string{
		"--model-tries", strconv.Itoa(p.Tries),
		"--model-retry-backoff-ms", strconv.FormatInt(p.Backoff.Milliseconds(), 10),
		"--model-max-retry-after-ms", strconv.FormatInt(p.MaxRetryAfter.Milliseconds(), 10),
	}
}

// harnessRetries reports whether the harness retries model requests itself, so that
// agentd must not repeat the turn as well (one retrying layer, ADR 0056 amendment 2026-10-31).
func harnessRetries(f string) bool {
	d, _ := harness.Lookup(f)
	return d.RetriesModelRequests
}

// launchCommand is the harness's ACP launch command. Binaries are absolute paths: OpenShell
// does not pass the image's PATH to exec'd processes (ADR 0048).
func launchCommand(f string, e endpoints) ([]string, error) {
	d, ok := harness.Lookup(f)
	if !ok || d.EntryScript == "" {
		return nil, fmt.Errorf("harness %q has no ACP launch command", f)
	}
	cmd := []string{
		d.EntryScript,
		"--model-base-url", e.ModelBaseURL,
		"--model", e.Model,
		"--mcp-url", e.MCPURL,
	}
	// The harness is the retrying layer: it gets Koinon's policy, never runs on its own defaults.
	if d.RetriesModelRequests {
		if e.Retry.Tries < 1 || e.Retry.Backoff < 0 || e.Retry.MaxRetryAfter < 0 {
			return nil, fmt.Errorf("harness %q retries model requests and needs Koinon's retry policy, got %+v", f, e.Retry)
		}
		cmd = append(cmd, e.Retry.args()...)
	}
	if e.Format != "" && len(d.ModelEntries) > 0 {
		if _, ok := d.ModelEntries[e.Format]; !ok {
			return nil, fmt.Errorf("harness %q has no provider entry for the %q model format; add a row to its ModelEntries", f, e.Format)
		}
		if d.FormatArg != "" {
			cmd = append(cmd, d.FormatArg, string(e.Format))
		}
	}
	if e.Thinking != "" && d.ThinkingArg != "" {
		cmd = append(cmd, d.ThinkingArg, string(e.Thinking))
	}
	if e.CacheMarker != "" {
		arg, ok := d.CacheArgs[e.CacheMarker]
		if !ok {
			return nil, fmt.Errorf("harness %q cannot send %q prompt-cache markers; add its switch to the harness table", f, e.CacheMarker)
		}
		cmd = append(cmd, arg)
	}
	if e.SessionID != "" && d.SessionArg != "" {
		cmd = append(cmd, d.SessionArg, e.SessionID)
	}
	return cmd, nil
}

// readyCommand is the ACP command a harness announces once Koinon's MCP tools are
// registered, or "" when it announces none (harness.Descriptor.ReadyCommand).
func readyCommand(f string) string {
	d, _ := harness.Lookup(f)
	return d.ReadyCommand
}

// mcpToolPrefix is the title prefix of the harness's calls to Koinon's MCP tools, or "".
func mcpToolPrefix(f string) string {
	d, _ := harness.Lookup(f)
	return d.MCPToolPrefix
}

// modelOption is the value agentd selects for the session's model option
// (session/set_config_option), or "" when the harness takes its model from the launch command
// alone. OpenCode names models <provider>/<model>; the provider is the harness's entry for the
// model's format (harness.Descriptor.ModelEntries). An empty format means chat completions.
func modelOption(f string, format modelformat.Format, model string) string {
	d, _ := harness.Lookup(f)
	if format == "" {
		format = modelformat.OpenAI
	}
	if entry := d.ModelEntries[format]; entry != "" {
		return entry + "/" + model
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
// settings ({"harness": {"pluginInstalls": true}}, copied from the template). Only harnesses
// with the option have one (OpenCode: --plugin-installs lets it load external plugins,
// flavors/opencode/README.md); the reconciler opens the npm registry for the same agents.
//
// A harness with built-in web tools also gets its NoWebToolsArg unless the version has web egress
// (agentconfig.Config.WebEgress): a tool the sandbox's network policy would only fail is not
// offered, so the model uses the org's MCP tools instead (ADR 0080).
func harnessArgs(f string, c agentconfig.Config) []string {
	d, _ := harness.Lookup(f)
	var args []string
	if c.Harness.PluginInstalls && d.PluginInstalls {
		args = append(args, d.PluginInstallsArg)
	}
	if d.NoWebToolsArg != "" && !c.WebEgress() {
		args = append(args, d.NoWebToolsArg)
	}
	return args
}

// cacheMarker is the cache-marker format to start the harness with for a model (ADR 0068): ""
// when the model has no cache price or its provider caches by itself. A model with a cache
// price but an unmapped provider format runs uncached (the catalog test keeps the default list
// free of those), and the second result says so for the log. A native-format model (CacheNative)
// needs no switch either: the harness places its markers.
func cacheMarker(mode prices.CacheMode) (marker string, unknown bool) {
	return mode.Marker(), mode.Uncached()
}
