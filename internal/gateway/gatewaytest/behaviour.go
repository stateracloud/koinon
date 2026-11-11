// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above
)

// The behaviour suite (ADR 0097 decision 4, step 1) states what Koinon needs from a gateway, through
// [Env] and [Gateway] and what a client sees, never through agentgateway's configuration. Two
// seams carry what a client cannot see: [Gateway.Usage], the usage record the spend ledger reads
// (gateway.UsageRecord, ADR 0097 decision 3), and [KnownGap], a case the driver's gateway fails and
// a named open issue tracks.

// AgentgatewayV170Gaps are the cases agentgateway v1.7.0 fails today, each with the open issue that
// tracks it. A driver for that gateway sets it as [Env.KnownGaps]; a driver without these gaps (a
// gateway of Koinon's own, ADR 0097 step 2) leaves it nil and the same cases run plainly. A case is
// written as [KnownGap](env, "#N", ...): a gateway bump that fixes a gap fails the case ("now
// passes"), and the fix removes the entry and the wrapper; a bump that adds a gap adds an entry,
// reviewed like code (ADR 0097 decision 4). The issues are in stateracloud/koinon.
var AgentgatewayV170Gaps = map[string]string{
	"#78": "agentgateway v1.7.0 ignores Messages output_tokens_details.thinking_tokens, so the thinking split is never recorded for Claude",
	"#550": "agentgateway v1.7.0 records a provider's mid-stream error event, and a stream the client cancels, with status 200 and " +
		"no error reason: the record is not marked failed, so the ledger charges the partial usage as a completed call",
	"#254": "agentgateway v1.7.0 sends the leak guard only tools/call replies: tools/list descriptions, JSON-RPC error " +
		"replies, notifications and server-initiated requests reach the agent unseen (upstream agentgateway#4245)",
}

// KnownGap runs check. When env.KnownGaps lists issue, the gateway has that gap: check must fail,
// and the case fails when it passes (the gap is closed: remove it). When the driver does not list
// issue, check runs plainly.
func KnownGap(env Env, issue string, check func()) {
	GinkgoHelper()
	why, ok := env.KnownGaps[issue]
	if !ok {
		check()
		return
	}
	failures := InterceptGomegaFailures(check)
	if len(failures) == 0 {
		Fail(fmt.Sprintf("expected failure %s now passes (%s): remove its KnownGap wrapper and its entry in the driver's KnownGaps", issue, why))
	}
	AddReportEntry("expected failure "+issue, strings.Join(append([]string{why}, failures...), "\n"))
}
