// SPDX-License-Identifier: Apache-2.0

// Package agentgateway is the agentgateway side of the usage record (ADR 0097 decision 3): the
// access-log attributes the drivers render, and the OTLP receiver that maps agentgateway's exported
// access log to [gateway.UsageRecord]s for the spend ledger. The ledger imports none of it.
package agentgateway

import (
	"fmt"
	"strings"
)

// Access-log attributes both agentgateway drivers (standalone and kubernetes) add to agentgateway's
// access log. [Decode] maps them to usage records; nothing else reads them (ADR 0097 decision 2.5).
const (
	// LogFieldKey is the gateway key's ID (Key.ID).
	LogFieldKey = "koinon.key"
	// LogFieldAgent is the key's agent (AgentRef.String: "<org slug>/<agent>").
	LogFieldAgent = "koinon.agent"
	// LogFieldRequest is a per-request UUID. agentgateway logs its own request
	// id only at debug level, so drivers add one; the ledger is idempotent on it.
	LogFieldRequest = "koinon.request"

	// LogFieldInput and the fields after it carry the usage in agentgateway's consistent
	// semantics (llm.inputTokens includes cache reads and writes, llm.outputTokens includes
	// reasoning), so the usage record has disjoint buckets whatever the provider's convention.
	LogFieldInput      = "koinon.in"
	LogFieldOutput     = "koinon.out"
	LogFieldCacheRead  = "koinon.cache_read"
	LogFieldCacheWrite = "koinon.cache_write"
	// LogFieldCacheWrite1h is the 1-hour part of LogFieldCacheWrite. The drivers do not render it
	// yet: agentgateway exposes no CEL field for the 1-hour token count (#270).
	LogFieldCacheWrite1h = "koinon.cache_write_1h"
	LogFieldReasoning    = "koinon.reasoning"
	// LogFieldStatus is the HTTP status the gateway answered; a 2xx LLM record without usage
	// is a request whose usage must be estimated (an aborted stream, a provider that
	// reports none).
	LogFieldStatus = "koinon.status"
	// LogFieldMaxTokens is the request's max_tokens, to settle an aborted request that bills
	// hidden thinking.
	LogFieldMaxTokens = "koinon.max_tokens" //nolint:gosec // attribute name, not a credential
	// LogFieldFormat is the wire format of the request (a modelformat.Format value, "" for a path that
	// is not a model route), read from the request path alone, so the log needs no body. The usage
	// record carries it, and the ledger tells a cut Messages stream by it (ADR 0069 amendment).
	LogFieldFormat = "koinon.format"
	// LogFieldStream is llm.streaming: whether the request streamed its response.
	LogFieldStream = "koinon.stream"
	// LogFieldError is proxy.error.reason: why the proxy failed the request, absent when it did not.
	// A response that was cut mid-stream is logged with its 200 status and the reason here.
	LogFieldError = "koinon.error"
	// LogFieldRequestBytes is the request's Content-Length: the byte count the estimate and its
	// calibration use.
	LogFieldRequestBytes = "koinon.req_bytes"
	// LogFieldResponseID is the provider's response ID from a response header (an OpenRouter
	// generation ID, x-generation-id), so the ledger can ask the provider later what it billed for a
	// response cut short (ADR 0056, amendment 2026-10-28). See ResponseIDCEL.
	LogFieldResponseID = "koinon.response_id"
	// LogFieldUpstream is whether the gateway called the provider for this request (a boolean,
	// UpstreamCEL). It tells a status the provider answered from one the gateway made itself (a bad
	// or revoked key, a model the key may not use, its own budget): the ledger charges and attributes
	// the first and ignores the second (ADR 0056, amendment 2026-10-29). A record without the field
	// comes from a config that predates it.
	LogFieldUpstream = "koinon.upstream"
)

// UpstreamCEL is the CEL of LogFieldUpstream. proxy.upstreamDuration is the time spent waiting for
// the outbound call, so it exists exactly when the gateway made one (verified against v1.7.0: a
// provider's 403 has it, a gateway 401 or model_not_allowed 403 does not).
const UpstreamCEL = `default(proxy.upstreamDuration, "") != ""`

// ResponseIDHeaders are the response headers that carry a provider's response ID, in the order
// tried. OpenRouter returns x-generation-id on every endpoint; the ID is what its generation
// endpoint takes (package providerusage).
var ResponseIDHeaders = []string{"x-generation-id"}

// ResponseIDCEL is the CEL of LogFieldResponseID: the first of ResponseIDHeaders the response
// carries. It reads a header only, so the gateway records no body for it (a body variable in a
// log expression would make agentgateway record the whole response as it streams). A response
// without such a header leaves the field out (a failed expression is left out of the record).
var ResponseIDCEL = func() string {
	parts := make([]string, len(ResponseIDHeaders))
	for i, h := range ResponseIDHeaders {
		parts[i] = fmt.Sprintf("response.headers[%q]", h)
	}
	return "coalesce(" + strings.Join(parts, ", ") + ")"
}()
