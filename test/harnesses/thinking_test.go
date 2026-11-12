// SPDX-License-Identifier: Apache-2.0

package harnesses_test

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

// ADR 0069 amendments 2026-10-29 (old issue 240) and 2026-10-31 (old issue 316): Koinon decides what a request says
// about thinking, and neither OpenCode's model-ID regex nor a provider default does. OpenCode 2.2.0
// adds adaptive thinking with block_binding for every ID that parses as Claude 5.1 or later (rc.7
// sent that to Anthropic without its beta and every turn failed), and direct Anthropic hides Claude
// 5.x thinking unless the request asks for a summary (rc.9). The fake model runs in strict mode, so
// a body field the model does not take fails the spec as it fails upstream.

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

// modelPath is the path a model of the format is posted to.
func modelPath(format modelformat.Format) string {
	if format == modelformat.Anthropic {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

// wantBody is the exact thinking field each form puts in the body: the key and its value, or no key
// at all.
var wantBody = map[thinking.Mode]struct {
	key   string
	value map[string]any
}{
	thinking.AdaptiveSummarized: {"thinking", map[string]any{"type": "adaptive", "display": "summarized"}},
	thinking.Disabled:           {"thinking", map[string]any{"type": "disabled"}},
	thinking.EffortNone:         {"reasoning", map[string]any{"effort": "none"}},
	thinking.Default:            {},
	thinking.None:               {},
}

var _ = Describe("koinon-opencode with --thinking", func() {
	bodies := func(gw *gateway, format modelformat.Format) []map[string]any {
		GinkgoHelper()
		var out []map[string]any
		for _, r := range requestsOn(gw, modelPath(format)) {
			var body map[string]any
			Expect(json.Unmarshal(r.Body, &body)).To(Succeed())
			out = append(out, body)
		}
		Expect(out).NotTo(BeEmpty())
		return out
	}

	run := func(ctx SpecContext, model string, format modelformat.Format, extra ...string) *gateway {
		GinkgoHelper()
		script := contractScript(opencode)
		script.Model = model
		gw := newGateway(script)
		gw.model.SetStrictMessages(true)
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		h, session := harnessFor(ctx, sb, gw, model, format, "", extra...)
		_, err := h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(requestsOn(gw, modelPath(format))).NotTo(BeEmpty())
		return gw
	}

	// Every catalog model on each route it is served on, with the form internal/thinking decides for
	// thinking on and, where the model can stop, for thinking off. A model that always thinks has no
	// off run: the API refuses "off" for it, and a stored "off" runs the on form.
	type route struct {
		name, provider, model string
		format                modelformat.Format
	}
	routes := []route{
		{"direct Haiku 5.5", "anthropic", "claude-haiku-5-5", modelformat.Anthropic},
		{"OpenRouter Haiku 5.5", "openrouter", "anthropic/claude-haiku-5.5", modelformat.Anthropic},
		{"direct Sonnet 5.5", "anthropic", "claude-sonnet-5-5", modelformat.Anthropic},
		{"OpenRouter Sonnet 5.5", "openrouter", "anthropic/claude-sonnet-5.5", modelformat.Anthropic},
		{"direct Opus 5.5", "anthropic", "claude-opus-5-5", modelformat.Anthropic},
		{"direct Fable 5.1", "anthropic", "claude-fable-5-1", modelformat.Anthropic},
		{"OpenRouter Fable 5.1", "openrouter", "anthropic/claude-fable-5.1", modelformat.Anthropic},
		{"OpenRouter GPT-6 Luna", "openrouter", "openai/gpt-6-luna", modelformat.OpenAI},
		{"OpenRouter DeepSeek v4.1 flash", "openrouter", "deepseek/deepseek-v4.1-flash", modelformat.OpenAI},
	}

	DescribeTable("sends the exact form Koinon decided, and no beta",
		func(ctx SpecContext, r route, mode thinking.Mode) {
			gw := run(ctx, r.model, r.format, "--thinking", string(mode))
			want := wantBody[mode]
			for _, body := range bodies(gw, r.format) {
				for _, k := range []string{"thinking", "reasoning"} {
					if k == want.key {
						Expect(body).To(HaveKeyWithValue(k, Equal(want.value)), "the whole %s object, nothing added", k)
					} else {
						Expect(body).NotTo(HaveKey(k))
					}
				}
				Expect(body).NotTo(HaveKey("output_config"), "per-turn effort adds a body field and a beta of its own")
			}
			if r.format == modelformat.Anthropic {
				// OpenCode always asks for interleaved thinking; with its own block off it adds no binding
				// beta. In a run the gateway drops the client's header, since no row pins one.
				Expect(betasSeen(gw)).To(Equal([]string{interleavedBeta}))
			}
		},
		func() []TableEntry {
			var es []TableEntry
			for _, r := range routes {
				c := thinking.Of(r.provider, r.model, r.format)
				if !c.Supported() {
					panic(r.name + ": the thinking table says the model does not think")
				}
				es = append(es, Entry(r.name+": on, "+string(c.Effective(thinking.On)), SpecTimeout(4*time.Minute), r, c.Effective(thinking.On)))
				if c.CanTurnOff() {
					es = append(es, Entry(r.name+": off, "+string(c.Effective(thinking.Off)), SpecTimeout(4*time.Minute), r, c.Effective(thinking.Off)))
				}
			}
			return es
		}(),
	)

	DescribeTable("none, or no switch, sends no thinking field whatever the ID",
		func(ctx SpecContext, model string, args ...string) {
			gw := run(ctx, model, modelformat.Anthropic, args...)
			for _, body := range bodies(gw, modelformat.Anthropic) {
				Expect(body).NotTo(HaveKey("thinking"))
				Expect(body).NotTo(HaveKey("output_config"))
			}
			Expect(betasSeen(gw)).To(Equal([]string{interleavedBeta}))
		},
		func() []TableEntry {
			var es []TableEntry
			// A Claude ID OpenCode would give a thinking block, an older Claude and the contract ID.
			for _, id := range []string{"claude-haiku-5-5", "claude-sonnet-4-5", contractModel} {
				es = append(es,
					Entry(id+" with --thinking none", SpecTimeout(4*time.Minute), id, "--thinking", "none"),
					Entry(id+" with no --thinking switch: not OpenCode's guess", SpecTimeout(4*time.Minute), id))
			}
			return es
		}(),
	)

	It("refuses --thinking with a value it does not know, and a form on the other format", func(ctx SpecContext) {
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		base := opencode.command("http://gw.test/v1", "m-1", "http://gw.test/mcp")
		for _, extra := range [][]string{
			{"--thinking", "high"},
			{"--thinking", "adaptive"},
			{"--thinking"},
			{"--model-format", "openai", "--thinking", "adaptive-summarized"},
			{"--model-format", "openai", "--thinking", "disabled"},
			{"--model-format", "anthropic", "--thinking", "effort-none"},
		} {
			_, stderr, code := sb.try(ctx, append(slices.Clone(base), extra...)...)
			Expect(code).To(Equal(64), "%v: %s", extra, stderr)
			Expect(stderr).To(ContainSubstring("koinon-opencode:"), "%v", extra)
		}
	}, SpecTimeout(2*time.Minute))

	It("knows every mode of internal/thinking", func(ctx SpecContext) {
		Expect(thinking.Modes()).To(ConsistOf(
			thinking.None, thinking.Default, thinking.AdaptiveSummarized, thinking.Disabled, thinking.EffortNone),
			"a new mode needs a case in the entry script and a row in wantBody")
		for _, m := range thinking.Modes() {
			Expect(wantBody).To(HaveKey(m))
		}
	})
})
