// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"slices"
	"strconv"
	"strings"
)

// ForwardedHeaders are the client request headers an LLM route passes to the model's upstream
// (lower-cased). Every other header the client sent is dropped, so the gateway key an agent holds
// cannot reach a third party's logs through a header the agent invented (#262). The headers the
// gateway needs upstream are not here: it sets them itself (Rendering.Headers, the provider's
// credential, OrgHeader).
//
// content-length stays so the access log's request size (LogFieldRequestBytes) still reads it.
// Sticky routing needs no header: the session ID travels as a body field (flavors/opencode).
var ForwardedHeaders = []string{"accept", "content-length", "content-type", "user-agent"}

// MCPForwardedHeaders are the client request headers an MCP route passes to a target. The session
// and protocol headers belong to the gateway's own MCP handling, which reads them after the
// transformation runs, so dropping them would break every session.
var MCPForwardedHeaders = []string{
	"accept", "content-length", "content-type", "last-event-id", "mcp-protocol-version", "mcp-session-id", "user-agent",
}

// AllowHeadersExpr is the CEL expression that evaluates to the request headers named in allowed
// (lower-cased) and no others, for agentgateway's transformation `replace`. If it fails to
// evaluate, agentgateway leaves the headers untouched, so it uses nothing but the headers map
// and string literals.
func AllowHeadersExpr(allowed []string) string {
	names := slices.Sorted(slices.Values(allowed))
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(strings.ToLower(n))
	}
	return "request.headers.filterKeys(k, k in [" + strings.Join(quoted, ", ") + "])"
}

// NoKeyInURLExpr is the CEL condition under which the request URL (path and query, percent-decoded)
// carries no gateway key: a key is a header credential, and a URL is logged by every hop. An
// undecodable URL fails the condition, which refuses the request.
func NoKeyInURLExpr() string {
	return "!url.decode(request.pathAndQuery).contains(" + strconv.Quote(KeyPrefix) + ")"
}

// NoKeyInHeadersExpr is the CEL condition under which no request header but authorization (the one
// that carries the key to the gateway) holds a gateway key. It is the Kubernetes driver's guard in
// place of AllowHeadersExpr: agentgateway v1.7's AgentgatewayPolicy CRD has no way to replace the
// header set (its data plane does, which the standalone driver uses), so an unknown header cannot be
// dropped there, only refused when it carries a key. It stops the accident and the plain attack; a
// key re-encoded by the agent evades it, which only the allowlist closes.
func NoKeyInHeadersExpr() string {
	return `!toJson(request.headers.filterKeys(k, k != "authorization")).contains(` + strconv.Quote(KeyPrefix) + ")"
}
