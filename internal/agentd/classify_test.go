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

	DescribeTable("errors that may be the gateway refusing a spent budget's key",
		func(err error, gateway bool) { Expect(agentd.ClassifyGateway(err)).To(Equal(gateway)) },
		Entry("a structured 401", rpc("Authentication required", map[string]any{"statusCode": 401}), true),
		Entry("OpenCode's 401 without a status",
			rpc("Authentication required: provider authentication required", nil), true),
		Entry("a structured 429", rpc("slow down", map[string]any{"status": 429}), true),
		Entry("a 429 in the text", errors.New("429 Too Many Requests"), true),
		Entry("a provider's rate limit phrase", errors.New("rate limit exceeded"), true),
		Entry("a 403", rpc("denied", map[string]any{"statusCode": 403}), false),
		Entry("a 503", rpc("boom", map[string]any{"statusCode": 503}), false),
		Entry("an unknown error", errors.New("nope"), false),
	)

	DescribeTable("errors the gateway's record can improve (an authentication failure worded by the harness)",
		func(err error, want bool) { Expect(agentd.IsAuthLike(err)).To(Equal(want)) },
		Entry("OpenCode's error without a status", rpc("Authentication required: provider authentication required", nil), true),
		Entry("a structured 401", rpc("Authentication required", map[string]any{"statusCode": 401}), true),
		Entry("a 403 names itself", rpc("denied", map[string]any{"statusCode": 403}), false),
		Entry("a 429", rpc("slow down", map[string]any{"status": 429}), false),
		Entry("an unknown error", errors.New("nope"), false),
	)

	Describe("the gateway's record of the provider refusing the request", func() {
		opencode := rpc("Authentication required: provider authentication required", nil)

		It("makes a provider 403 a policy failure, not a gateway-key fault", func() {
			class, gateway, reason := agentd.RefusedByProvider(opencode, 403, "403: Input flagged by moderation (reasons: violence)", "openrouter")
			Expect(class).To(Equal("policy"))
			Expect(gateway).To(BeFalse(), "a spent budget's check must not take it")
			Expect(reason).To(HavePrefix("The model provider (openrouter) refused the request (HTTP 403): 403: Input flagged by moderation (reasons: violence)"))
			Expect(reason).To(ContainSubstring("Authentication required: provider authentication required"), "the harness's own words stay for the record")
		})

		It("makes a provider 401 a provider-key fault, fatal, not the agent's gateway key", func() {
			class, gateway, reason := agentd.RefusedByProvider(opencode, 401, "", "anthropic")
			Expect(class).To(Equal("fatal"))
			Expect(gateway).To(BeFalse())
			Expect(reason).To(And(ContainSubstring("rejected the API key Koinon holds for it (HTTP 401)"), ContainSubstring("provider's key in the catalog")))
			Expect(reason).NotTo(ContainSubstring("gateway key"))
		})

		It("names the provider only when it is known", func() {
			_, _, reason := agentd.RefusedByProvider(opencode, 403, "", "")
			Expect(reason).To(HavePrefix("The model provider refused the request (HTTP 403) ["))
		})
	})

	DescribeTable("ACP stop reasons",
		func(stop acp.StopReason, want string) { Expect(agentd.StopClass(stop)).To(Equal(want)) },
		Entry("refusal is policy", acp.StopRefusal, "policy"),
		Entry("max_tokens is budget", acp.StopMaxTokens, "budget"),
		Entry("max_turn_requests is budget", acp.StopMaxTurnRequests, "budget"),
		Entry("anything else is fatal", acp.StopReason("weird"), "fatal"),
	)
})
