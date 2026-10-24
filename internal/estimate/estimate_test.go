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
		estimate.Input{ReportedInput: ptr(1000), ResponseBytes: 459, Aborted: true})
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

func TestHiddenThinkingAbortSettlesAtMaxTokens(t *testing.T) {
	p := estimate.Params{Tokenizer: pricing.TokenizerBytes, Ratio: 2.3, HiddenThinking: true}
	r := estimate.Estimate(p, estimate.Input{RequestBytes: 230, ResponseBytes: 10, Aborted: true, MaxTokens: 8000})
	if !r.ThinkingUnmeasured || r.OutputTokens != 8000 || r.Method != "max_tokens" {
		t.Fatalf("%+v", r)
	}
	// A request that completed without usage is estimated from its visible text.
	r = estimate.Estimate(p, estimate.Input{RequestBytes: 230, ResponseBytes: 229})
	if r.ThinkingUnmeasured || r.OutputTokens != 100 {
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
	r := estimate.Estimate(estimate.Params{Tokenizer: pricing.TokenizerO200K, Ratio: 3}, estimate.Input{RequestText: "hello world", RequestBytes: 11, ReportedOutput: ptr(1)})
	if r.InputTokens != 2 || r.Method != "tiktoken" {
		t.Fatalf("%+v", r)
	}
}
