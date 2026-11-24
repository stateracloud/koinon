// SPDX-License-Identifier: Apache-2.0

// Package bypass must not compile. Each function is code that would send a credential that the scrub did not
// record (#1221). The spec in outbound_type_test.go builds this package and expects one error for each.
package bypass

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// HTTPClient builds the outbound client with a source of its own.
func HTTPClient() {
	_, _ = mcpgateway.NewTargetHTTPClient(nil, gateway.MCPTarget{}, gateway.AgentRef{}, mcpgateway.StaticCredentials{}) // want: bypass-http-client
}

// A2A gives the A2A client a source of its own.
func A2A() {
	_ = a2aout.Options{Credentials: mcpgateway.StaticCredentials{}} // want: bypass-a2a
}

// ownSource is a built-in whose Call takes any source, so the gateway could not hand it the scrubbed one.
type ownSource struct{}

func (ownSource) Tools() []*mcp.Tool { return nil }

func (ownSource) Call(context.Context, mcpgateway.CredentialSource, gateway.MCPTarget, gateway.AgentRef, string, json.RawMessage) mcpgateway.BuiltinResult {
	return mcpgateway.BuiltinResult{}
}

var _ mcpgateway.Builtin = ownSource{} // want: bypass-builtin

// Wrap puts a source of its own in the scrubbed type without a scrub.
func Wrap() mcpgateway.ScrubbedSource {
	return mcpgateway.ScrubbedSource(mcpgateway.StaticCredentials{}) // want: bypass-convert
}

// Literal sets the field of the scrubbed type from outside the package.
func Literal() mcpgateway.ScrubbedSource {
	return mcpgateway.ScrubbedSource{r: nil} // want: bypass-literal
}
