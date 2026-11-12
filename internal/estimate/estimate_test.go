// SPDX-License-Identifier: Apache-2.0

package estimate_test

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/estimate"
	"github.com/stateracloud/koinon/pricing"
)

func ptr(n int64) *int64 { return &n }

func TestReportedCountsAreNotEstimates(t *testing.T) {
	r := estimate.Estimate(estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3},
		estimate.Input{ReportedInput: ptr(1200), ReportedOutput: ptr(40), RequestBytes: 9999})
	if r.Estimated || r.InputTokens != 1200 || r.OutputTokens != 40 || r.Method != "reported" {
		t.Fatalf("%+v", r)
	}
}

func TestEarlyInputOnlyOutputEstimated(t *testing.T) {
	// message_start gave the input; the stream was aborted after 460 visible bytes.
	r := estimate.Estimate(estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3},
		estimate.Input{ReportedInput: ptr(1000), ResponseBytes: 459})
	if !r.Estimated || !r.InputReported || r.InputTokens != 1000 || r.OutputTokens != 200 || r.Method != "bytes" {
		t.Fatalf("%+v", r)
	}
}

func TestBytesRatioAndOverheads(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 3.0, ToolOverheadTokens: 286}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 3001, HasTools: true, Images: []estimate.Image{{Width: 56, Height: 29}}})
	// ceil(3001/3)=1001, +286 tools, +2*2 image tokens
	if r.InputTokens != 1001+286+4 || r.OverheadTokens != 290 {
		t.Fatalf("%+v", r)
	}
}

func TestAbortWithUnknownSizeChargesABoundedShareOfMaxTokensAndReservesMaxTokens(t *testing.T) {
	for _, hidden := range []bool{true, false} {
		p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3, HiddenThinking: hidden, MaxOutput: 128000}
		r := estimate.Estimate(p, estimate.Input{RequestBytes: 46, Aborted: true, MaxTokens: 8000})
		// A quarter of the 8000 bound is 2000, under the 4096 ceiling.
		if !r.Estimated || r.InputTokens != 20 || r.OutputTokens != 2000 || r.ReserveOutputTokens != 8000 ||
			r.Method != "partial" || r.OutputBasis != estimate.OutputBounded || r.ThinkingUnmeasured != hidden || r.MaxOutputFallback {
			t.Fatalf("hidden=%v %+v", hidden, r)
		}
	}
	// Input the gateway did log (partial usage) is used as reported.
	r := estimate.Estimate(estimate.Params{Ratio: 2.3, MaxOutput: 128000}, estimate.Input{ReportedInput: ptr(1000), Aborted: true, MaxTokens: 8000})
	if r.InputTokens != 1000 || !r.InputReported || r.OutputTokens != 2000 || r.ReserveOutputTokens != 8000 {
		t.Fatalf("%+v", r)
	}
}

// Old issue 670: an aborted chat-completions stream with max_tokens 32000 is not priced with zero
// output, and not at max_tokens either (ADR 0056, amendment 2026-10-31, point 2).
func TestAbortedCompletionsStreamIsNotPricedWithZeroOutput(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 3, MaxOutput: 64000}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 3000, Aborted: true, MaxTokens: 32000})
	if r.OutputTokens != estimate.UnknownOutputMax || r.ReserveOutputTokens != 32000 || r.OutputBasis != estimate.OutputBounded {
		t.Fatalf("%+v", r)
	}
}

func TestCutOutput(t *testing.T) {
	p := estimate.Params{Ratio: 3}
	for _, c := range []struct {
		name          string
		bytes         int64
		known         bool
		bound, tokens int64
		basis         string
	}{
		{"measured bytes", 3000, true, 32000, 1000, estimate.OutputFromBytes},
		{"measured bytes beyond the bound", 300_000, true, 32000, 32000, estimate.OutputFromBytes},
		{"nothing streamed", 0, true, 32000, 0, estimate.OutputNothingStreamed},
		{"unknown size, large bound", -1, false, 32000, estimate.UnknownOutputMax, estimate.OutputBounded},
		{"unknown size, small bound", -1, false, 2000, 500, estimate.OutputBounded},
		{"unknown size, bound rounds up", 0, false, 5, 2, estimate.OutputBounded},
		{"unknown size, no bound", 0, false, 0, 0, estimate.OutputBounded},
	} {
		tokens, basis := estimate.CutOutput(p, c.bytes, c.known, c.bound)
		if tokens != c.tokens || basis != c.basis {
			t.Errorf("%s: got %d %s, want %d %s", c.name, tokens, basis, c.tokens, c.basis)
		}
	}
}

func TestAbortWithMeasuredResponseChargesItsBytes(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3, MaxOutput: 128000}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 46, ResponseBytes: 459, ResponseKnown: true, Aborted: true, MaxTokens: 8000})
	if r.OutputTokens != 200 || r.ReserveOutputTokens != 8000 || r.OutputBasis != estimate.OutputFromBytes {
		t.Fatalf("%+v", r)
	}
	// A stream cut before anything was streamed is charged no output (amendment 2026-10-31, point 1).
	r = estimate.Estimate(p, estimate.Input{RequestBytes: 46, ResponseKnown: true, Aborted: true, MaxTokens: 8000})
	if r.OutputTokens != 0 || r.ReserveOutputTokens != 8000 || r.OutputBasis != estimate.OutputNothingStreamed {
		t.Fatalf("%+v", r)
	}
}

func TestAbortWithoutMaxTokensReservesTheEntrysMaxOutputAndFlagsIt(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3, MaxOutput: 128000}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 230, Aborted: true})
	if r.OutputTokens != estimate.UnknownOutputMax || r.ReserveOutputTokens != 128000 || !r.MaxOutputFallback || r.Method != "partial" || !r.Estimated {
		t.Fatalf("%+v", r)
	}
	// A response that completed without usage is estimated from its bytes and reserves nothing.
	r = estimate.Estimate(p, estimate.Input{RequestBytes: 230, ResponseBytes: 229})
	if r.OutputTokens != 100 || r.ReserveOutputTokens != 0 || r.MaxOutputFallback {
		t.Fatalf("%+v", r)
	}
}

func TestCalibrationOnlyRaises(t *testing.T) {
	p := estimate.Params{Ratio: 3.0, LearnedP90: 0.5} // 2 bytes per token learned: more tokens
	if p.EffectiveRatio() != 2.0 || p.Tokens(100) != 50 {
		t.Fatalf("%v %v", p.EffectiveRatio(), p.Tokens(100))
	}
	p.LearnedP90 = 0.2 // 5 bytes per token learned: never lowers the estimate
	if p.EffectiveRatio() != 3.0 {
		t.Fatalf("%v", p.EffectiveRatio())
	}
	p.LearnedP90 = 1e9 // a degenerate sample is capped at the lower bound of a ratio
	if p.EffectiveRatio() != estimate.MinRatio {
		t.Fatalf("%v", p.EffectiveRatio())
	}
}

func TestTiktokenOfflineAndExact(t *testing.T) {
	n, err := estimate.CountTiktoken("o200k_base", "hello world")
	if err != nil || n != 2 {
		t.Fatalf("o200k hello world = %d, %v", n, err)
	}
	n, err = estimate.CountTiktoken("cl100k_base", strings.Repeat("a", 10))
	if err != nil || n < 1 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestAbsurdMaxTokensIsCapped(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 3, MaxOutput: 128000}
	r := estimate.Estimate(p, estimate.Input{Aborted: true, MaxTokens: 9_000_000_000_000})
	if r.ReserveOutputTokens != 128000 || !r.MaxOutputCapped {
		t.Fatalf("%+v", r)
	}
	// Without a max_output the hard ceiling applies.
	r = estimate.Estimate(estimate.Params{Ratio: 3}, estimate.Input{Aborted: true, MaxTokens: 9_000_000_000_000})
	if r.ReserveOutputTokens != estimate.HardMaxOutput {
		t.Fatalf("%+v", r)
	}
	r = estimate.Estimate(estimate.Params{Ratio: 3}, estimate.Input{RequestBytes: 1 << 60, Aborted: true, MaxTokens: 5})
	if r.InputTokens != estimate.HardMaxInput || r.OutputTokens != 2 || r.ReserveOutputTokens != 5 {
		t.Fatalf("%+v", r)
	}
}

func TestRejectedRequestChargesInputOnly(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 3, MaxOutput: 128000}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 3000, Rejected: true, Aborted: true, MaxTokens: 4000})
	if r.InputTokens != 1000 || r.OutputTokens != 0 || !r.Estimated {
		t.Fatalf("%+v", r)
	}
}

func TestFailedCallChargesInputOnlyAndReservesNothing(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 3, MaxOutput: 128000}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 3000, Failed: true, Aborted: true, MaxTokens: 4000})
	if r.InputTokens != 1000 || r.OutputTokens != 0 || r.ReserveOutputTokens != 0 || !r.Estimated || r.Method != "bytes" {
		t.Fatalf("%+v", r)
	}
	// An aborted response that did not fail with a 5xx keeps its reserve (a cut stream after output).
	r = estimate.Estimate(p, estimate.Input{RequestBytes: 3000, Aborted: true, MaxTokens: 4000})
	if r.ReserveOutputTokens != 4000 {
		t.Fatalf("%+v", r)
	}
}
