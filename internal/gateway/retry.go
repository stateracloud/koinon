// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"strings"
)

// The do-not-retry marker (ADR 0056, 2026-11-01 amendment, points 4 and 6; old issue 342).
//
// Contract, for every consumer of a model or MCP response from the gateway:
//
//  1. A refusal the gateway itself generates (a bad or missing key, a revoked key, a key over its
//     budget or run cap, a model or tool the key may not use, a request the policy rejects) and a
//     403 from a provider or aggregator (a policy refusal) carry the response header
//     DoNotRetryHeader with the value DoNotRetryValue.
//  2. The status of a marked response is in 400..499. A marked response is final: no layer (the
//     harness, agentd, a Koinon client) retries it, and none honours its Retry-After.
//  3. agentgateway v1.7.0 answers some refusals in its key stage, before any response policy runs,
//     so they cannot carry the marker (verified on standalone and kind, gatewaytest Env.Unmarked):
//     a 401 for a missing, unknown or revoked key (both modes; Kubernetes mode refuses a key over
//     its budget this way, the ledger revokes it), and in standalone mode also the 429
//     budget_exceeded, the LLM route's authorization 403 and the MCP route's 401. A consumer treats
//     every 401 as final by its status, and a 429 that has a Retry-After beyond its cap as final
//     (the standalone budget 429 sends the seconds until the window ends).
//     Interim: tracked in #112; exit condition there (an agentgateway release that runs response
//     policies on key-stage refusals, which makes gatewaytest Env.Unmarked fail loudly).
//  4. A response without the marker is not a refusal. A provider's own 429, 5xx and network
//     failure stay retryable; a gateway failure that is not a refusal (5xx) carries no marker.
//
// The header is the harness-neutral one OpenCode and the Anthropic and OpenAI SDKs already honour
// ("x-should-retry: false" beats any status rule), so a harness needs no Koinon-specific code.
// Code that decides retries reads it with DoNotRetry and never parses a body.
const (
	// DoNotRetryHeader is the response header that carries the marker.
	DoNotRetryHeader = "x-should-retry"
	// DoNotRetryValue is its value on a refusal.
	DoNotRetryValue = "false"
)

// DoNotRetry reports whether h marks its response as a refusal that must not be retried.
func DoNotRetry(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get(DoNotRetryHeader)), DoNotRetryValue)
}

// DoNotRetryCEL is the CEL expression of the DoNotRetryHeader value, evaluated on the response by
// agentgateway's response transformation (llm, mcp and the Kubernetes Gateway policy alike).
//
// It is "false" for a refusal: a 403 (a gateway authorization refusal or an aggregator's policy
// refusal, ADR 0056 point 6), or any other 4xx the gateway answered without calling a provider
// (401 key, 404 model_not_found, 429 budget_exceeded). proxy.upstreamDuration exists exactly when
// the gateway made the outbound call (agentgateway.UpstreamCEL), so a provider's own 429 or 400 is not marked.
//
// For any other response it reads the response's own header, which fails to evaluate when the
// response has none, and agentgateway then leaves the header out: a provider's answer is passed
// on untouched. The expression reads only the status, the duration and a header: never
// response.body or response.bodyPrefix, which would make agentgateway buffer every response
// (AGENTS.md).
const DoNotRetryCEL = `(response.code == 403 || (response.code >= 400 && response.code < 500 && default(proxy.upstreamDuration, "") == "")) ? "` +
	DoNotRetryValue + `" : response.headers["` + DoNotRetryHeader + `"]`
