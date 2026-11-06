// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// The behaviour suite (ADR 0097 decision 4, step 1) states what Koinon needs from a gateway, through
// [Env] and [Gateway] and what a client sees, never through agentgateway's configuration. Two
// seams carry what a client cannot see: [Observed], the gateway's own record of a request, and
// [KnownGap], a case the current gateway fails and a named open issue tracks.

// Observed is what a gateway recorded about one request: the usage record the spend ledger reads,
// as a driver-neutral value. Counts follow agentgateway's consistent semantics, which Koinon's
// ledger prices: Input includes cache reads and writes, Output includes reasoning.
type Observed struct {
	// Format is "openai" (chat completions) or "anthropic" (Messages).
	Format string
	Stream bool
	// Status is the HTTP status the gateway answered. A stream that fails after its first byte is
	// still 200.
	Status int
	// Failed is true when the gateway failed the response itself (a stream cut mid-way), whatever
	// the status said. The counts are then what arrived before the cut.
	Failed bool

	Input, Output, CacheRead, CacheWrite int64
	// Reasoning is nil when the gateway recorded no thinking split (the ledger keeps it null, not 0).
	Reasoning *int64
}

// ObservedFromAgentgatewayLog reads agentgateway's access log (the koinon.* fields, gateway.LogField*)
// as [Observed] records of one key. It is the default of [Gateway.Observe].
func ObservedFromAgentgatewayLog(log, keyID string) []Observed {
	var out []Observed
	for _, rec := range accessLog(log, keyID) {
		num := func(k string) int64 {
			f, _ := rec[k].(float64)
			return int64(f)
		}
		o := Observed{
			Status: int(num(gateway.LogFieldStatus)),
			Input:  num(gateway.LogFieldInput), Output: num(gateway.LogFieldOutput),
			CacheRead: num(gateway.LogFieldCacheRead), CacheWrite: num(gateway.LogFieldCacheWrite),
		}
		o.Format, _ = rec[gateway.LogFieldFormat].(string)
		o.Stream, _ = rec[gateway.LogFieldStream].(bool)
		if e, _ := rec[gateway.LogFieldError].(string); e != "" {
			o.Failed = true
		}
		if _, ok := rec[gateway.LogFieldReasoning].(float64); ok {
			r := num(gateway.LogFieldReasoning)
			o.Reasoning = &r
		}
		out = append(out, o)
	}
	return out
}

// observed returns the records of keyID, from the driver's Observe or from its log.
func (g *Gateway) observed(ctx context.Context, keyID string) ([]Observed, error) {
	switch {
	case g.Observe != nil:
		return g.Observe(ctx, keyID)
	case g.Logs != nil:
		return ObservedFromAgentgatewayLog(g.Logs(ctx), keyID), nil
	}
	return nil, fmt.Errorf("the driver exposes neither Observe nor Logs")
}

// KnownGaps are the cases the pinned gateway fails today, each with the open issue that tracks it.
// A case is written as [KnownGap]("#N", ...) and N must be a key here, so this map is the one list
// a reviewer reads: a gateway bump that fixes a gap fails the case ("now passes"), and the fix
// removes the entry and the wrapper; a bump that adds a gap adds an entry, reviewed like code
// (ADR 0097 decision 4). The issues are in stateracloud/koinon.
var KnownGaps = map[string]string{
	"#78": "agentgateway v1.7.0 ignores Messages output_tokens_details.thinking_tokens, so the thinking split is never recorded for Claude",
	"#254": "agentgateway v1.7.0 sends the leak guard only tools/call replies: tools/list descriptions, JSON-RPC error " +
		"replies, notifications and server-initiated requests reach the agent unseen (upstream agentgateway#4245)",
}

// KnownGap runs check and expects it to fail, because the gateway has the gap issue names in
// [KnownGaps]. It fails when check passes (the gap is closed: remove it) or the issue is not listed.
func KnownGap(issue string, check func()) {
	GinkgoHelper()
	why, ok := KnownGaps[issue]
	Expect(ok).To(BeTrue(), "%s is not in gatewaytest.KnownGaps", issue)
	failures := InterceptGomegaFailures(check)
	if len(failures) == 0 {
		Fail(fmt.Sprintf("expected failure %s now passes (%s): remove its KnownGap wrapper and its entry in KnownGaps", issue, why))
	}
	AddReportEntry("expected failure "+issue, strings.Join(append([]string{why}, failures...), "\n"))
}
