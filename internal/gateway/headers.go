// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"slices"
	"strconv"
	"strings"
)

// ForwardedHeaders are the client request headers an LLM route passes to the model's upstream
// (lower-cased). Every other header the client sent is dropped, so the gateway key an agent holds
// cannot reach a third party's logs through a header the agent invented (old issue 262). The headers the
// gateway needs upstream are not here: it sets them itself (Rendering.Headers, the provider's
// credential, OrgHeader).
//
// content-length stays so the access log's request size (agentgateway.LogFieldRequestBytes) still reads it.
// Sticky routing needs no header: the session ID travels as a body field (harnesses/opencode).
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
// that carries the key to the gateway) holds a gateway key. Both drivers refuse a request that fails
// it, on LLM and MCP routes alike. The allowlist (AllowHeadersExpr) alone is not enough: the headers
// it keeps carry whatever the client put in them (an agent chooses its own User-Agent), and if the
// replace expression ever failed to evaluate agentgateway would forward every header, while a failed
// authorization rule refuses. In Kubernetes mode it is the only header guard: agentgateway v1.7's
// AgentgatewayPolicy CRD has no way to replace the header set (its data plane does, which the
// standalone driver uses), so an unknown header without a key passes there. A key the agent
// re-encodes first, or puts in a value that is not valid UTF-8 (agentgateway's CEL leaves such a
// header out of request.headers), evades this check; only the allowlist drops those.
func NoKeyInHeadersExpr() string {
	return `!toJson(request.headers.filterKeys(k, k != "authorization")).contains(` + strconv.Quote(KeyPrefix) + ")"
}
