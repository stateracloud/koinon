// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/thinking"
)

// ADR 0069 amendment 2026-10-29 (#240): Koinon decides whether a Messages request carries a thinking
// block, and OpenCode's model-ID regex never does. OpenCode 2.2.0 adds adaptive thinking with
// block_binding for every ID that parses as Claude 5.1 or later (rc.7 sent that to Anthropic
// without its beta and every turn failed). The fake model runs in strict Messages mode, so a body
// field that needs a beta fails the spec as it fails on api.anthropic.com.

const interleavedBeta = "interleaved-thinking-2025-05-14"

// betasSeen is every anthropic-beta value OpenCode sent on its Messages requests.
func betasSeen(gw *gateway) []string {
	seen := map[string]bool{}
	for _, r := range requestsOn(gw, "/v1/messages") {
		for _, v := range r.Header.Values("Anthropic-Beta") {
			for _, b := range strings.Split(v, ",") {
				seen[strings.TrimSpace(b)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

var _ = Describe("koinon-opencode with --thinking", func() {
	// The IDs are real Claude IDs, in the shapes a direct Anthropic provider and OpenRouter use, an
	// older Claude that OpenCode does not give a binding, and the contract suite's fake ID.
	ids := []string{"claude-haiku-5-5", "anthropic/claude-haiku-5.5", "claude-opus-5-5", "claude-sonnet-4-5", contractModel}

	messagesBodies := func(gw *gateway) []map[string]any {
		var out []map[string]any
		for _, r := range requestsOn(gw, "/v1/messages") {
			var body map[string]any
			Expect(json.Unmarshal(r.Body, &body)).To(Succeed())
			out = append(out, body)
		}
		Expect(out).NotTo(BeEmpty())
		return out
	}

	run := func(ctx SpecContext, model string, extra ...string) *gateway {
		GinkgoHelper()
		script := contractScript(opencode)
		script.Model = model
		gw := newGateway(script)
		gw.model.SetStrictMessages(true)
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		h, session := harnessFor(ctx, sb, gw, model, modelformat.Anthropic, "", extra...)
		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(requestsOn(gw, "/v1/messages")).NotTo(BeEmpty())
		return gw
	}

	DescribeTable("thinking off sends no thinking block and no beta that needs one, whatever the ID",
		func(ctx SpecContext, model string, args ...string) {
			gw := run(ctx, model, args...)
			for _, body := range messagesBodies(gw) {
				Expect(body).NotTo(HaveKey("thinking"))
				Expect(body).NotTo(HaveKey("output_config"), "per-turn effort adds a body field and a beta of its own")
			}
			// OpenCode always asks for interleaved thinking, which the API ignores where it does not
			// apply; the gateway replaces the header with the model's pinned betas.
			Expect(betasSeen(gw)).To(Equal([]string{interleavedBeta}))
		},
		func() []TableEntry {
			var es []TableEntry
			for _, id := range ids {
				es = append(es,
					Entry(id+" with --thinking none", SpecTimeout(4*time.Minute), id, "--thinking", "none"),
					Entry(id+" with no --thinking switch: off, not OpenCode's guess", SpecTimeout(4*time.Minute), id))
			}
			return es
		}(),
	)

	DescribeTable("thinking on sends adaptive thinking with the binding beta, whatever the ID",
		func(ctx SpecContext, model string) {
			gw := run(ctx, model, "--thinking", "adaptive")
			for _, body := range messagesBodies(gw) {
				Expect(body).To(HaveKeyWithValue("thinking", HaveKeyWithValue("type", "adaptive")))
				Expect(body).NotTo(HaveKey("output_config"))
			}
			// The beta that goes with block_binding. In a run the gateway pins it from the catalog
			// (thinking.BindingBeta) and drops the client's header.
			Expect(betasSeen(gw)).To(Equal([]string{interleavedBeta, thinking.BindingBeta}))
		},
		func() []TableEntry {
			var es []TableEntry
			for _, id := range ids {
				es = append(es, Entry(id, SpecTimeout(4*time.Minute), id))
			}
			return es
		}(),
	)

	It("refuses --thinking with a value it does not know, and adaptive on chat completions", func(ctx SpecContext) {
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		base := opencode.command("http://gw.test/v1", "m-1", "http://gw.test/mcp")
		for _, extra := range [][]string{
			{"--thinking", "high"},
			{"--thinking"},
			{"--model-format", "openai", "--thinking", "adaptive"},
		} {
			_, stderr, code := sb.try(ctx, append(slices.Clone(base), extra...)...)
			Expect(code).To(Equal(64), "%v: %s", extra, stderr)
			Expect(stderr).To(ContainSubstring("koinon-opencode:"), "%v", extra)
		}
	}, SpecTimeout(2*time.Minute))
})
