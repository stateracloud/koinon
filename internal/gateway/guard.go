// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The MCP leak guard (ADR 0084, ADR 0091) runs in koinon-api behind agentgateway's mcpGuardrails: a
// remote ExtMcp processor on the MCP backend, for tools/call in both phases, failureMode
// FailClosed. Both drivers render it from these definitions.

// GuardMethod is the only JSON-RPC method sent to the guard, in both phases ("Full").
const GuardMethod = "tools/call"

// GuardMetadata is the processor's metadata: CEL expressions the gateway evaluates per call and
// sends in McpRequest.metadata_context (internal/mcpguard reads them). An expression that fails
// is left out, and the guard refuses a call without an agent.
func GuardMetadata() map[string]string {
	return map[string]string{
		"agent": "apiKey.agent",
		"tool":  "mcp.tool.name",
	}
}

// GuardAllowedHeaders are the only request headers forwarded to the guard. agentgateway forwards
// every header when the list is empty, the agent's gateway key among them; the guard needs none.
var GuardAllowedHeaders = []string{"mcp-protocol-version"}

// GuardDisallowedHeaders are never forwarded, whatever the allow list says.
var GuardDisallowedHeaders = []string{"authorization", "x-api-key", "cookie"}

// GuardUnconfiguredReason is the refusal the gateway gives every call to an org's MCP server when
// the reconciler has no guard address: the guard fails closed even when it is not deployed.
const GuardUnconfiguredReason = "refused: the MCP leak guard is not configured on this installation"

// GuardUnconfiguredCondition is true for a call to one of the org targets in s, so Koinon's own
// tools keep working without a guard. False when s has no org target.
func GuardUnconfiguredCondition(s State) string {
	var names []string
	for _, t := range s.MCP {
		if t.Org != "" {
			names = append(names, strconv.Quote(t.Name))
		}
	}
	if len(names) == 0 {
		return "false"
	}
	sort.Strings(names)
	return "mcp.tool.target in [" + strings.Join(names, ", ") + "]"
}

// ValidateGuardURL accepts "" (no guard) or an http(s) URL with a host and port and nothing after
// them, as agentgateway's CRD requires of a remote processor's url.
func ValidateGuardURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Port() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "/") {
		return fmt.Errorf("%q must be http(s)://host:port with no path", raw)
	}
	return nil
}

// GuardHostPort is the "host:port" of a valid guard URL, for the standalone config.
func GuardHostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
