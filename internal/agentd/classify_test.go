// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"errors"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentd"
)

var _ = Describe("Error classification", func() {
	rpc := func(msg string, data map[string]any) error {
		return &sdk.RequestError{Code: -32603, Message: msg, Data: data}
	}

	DescribeTable("prompt errors",
		func(err error, want string) {
			got, _ := agentd.ClassifyError(err)
			Expect(got).To(Equal(want))
		},
		Entry("a structured 400 that mentions a timeout is not retried",
			rpc("invalid_request: the timeout field must be positive", map[string]any{"statusCode": 400}), "fatal"),
		Entry("a structured 400 that mentions a 5xx-looking number is not retried",
			rpc("invalid_request: max 500 tokens", map[string]any{"statusCode": 400}), "fatal"),
		Entry("a structured 503 is transient", rpc("boom", map[string]any{"statusCode": 503}), "transient"),
		Entry("a structured 429 is throttled", rpc("slow down", map[string]any{"status": 429}), "throttled"),
		Entry("context_length_exceeded overrides the status",
			rpc("context_length_exceeded", map[string]any{"statusCode": 400}), "context_overflow"),
		Entry("insufficient_quota overrides the status",
			rpc("insufficient_quota", map[string]any{"statusCode": 429}), "budget"),
		Entry("without a status, the text decides: timeout", errors.New("request timed out"), "transient"),
		Entry("without a status, the text decides: 529", errors.New("HTTP 529 overloaded"), "transient"),
		Entry("unknown errors are fatal", errors.New("nope"), "fatal"),
	)

	DescribeTable("ACP stop reasons",
		func(stop acp.StopReason, want string) { Expect(agentd.StopClass(stop)).To(Equal(want)) },
		Entry("refusal is policy", acp.StopRefusal, "policy"),
		Entry("max_tokens is budget", acp.StopMaxTokens, "budget"),
		Entry("max_turn_requests is budget", acp.StopMaxTurnRequests, "budget"),
		Entry("anything else is fatal", acp.StopReason("weird"), "fatal"),
	)
})
