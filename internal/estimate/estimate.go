// SPDX-License-Identifier: Apache-2.0

// Package estimate estimates token usage a provider did not report (plan item PV2, ADR 0056
// amendment 2026-10-24; research: docs/research/claude-token-estimation.md).
//
// The order of preference, per side of the request:
//
//  1. Counts the provider did report, even early ones (Anthropic's message_start input_tokens),
//     are used as reported and are not estimates.
//  2. Otherwise UTF-8 bytes divided by the price entry's bytes-per-token ratio (Claude before
//     Opus 4.7: 3.0, Opus 4.7 and later: 2.3, others 3.0), plus fixed overheads for tool
//     definitions, images and PDF pages. Nobody ships an exact offline tokenizer for current
//     Claude models, and the tokenizers that exist undercount them, so none is used for Claude.
//  3. An aborted response without usage settles its output at the request's max_tokens, marked
//     estimated, whether or not the model bills hidden thinking (visible text cannot measure
//     thinking, and partial text would undercount). When max_tokens was not logged (OpenAI's
//     max_completion_tokens), the price entry's max_output stands in and the entry is flagged
//     MaxOutputFallback.
//  4. For OpenAI-family models, [CountTiktoken] counts text exactly with the offline encoding
//     (BPE files compiled into the binary, github.com/pkoukk/tiktoken-go-loader, MIT; nothing is
//     downloaded at run time). Koinon never logs or buffers prompt or response text, so the live
//     path does not use it; it is for callers that already hold the text.
//  5. Calibration only raises: the effective ratio is the more conservative of the static ratio
//     and the p90 of tokens per byte learned from real provider-reported usage.
//
// Estimates are marked, count toward limits and are shown separately.
package estimate

import (
	"fmt"
	"math"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
)

// Params come from the matched price entry and the calibration.
type Params struct {
	Tokenizer string  // pricing.Tokenizer*
	Ratio     float64 // static UTF-8 bytes per token
	// LearnedP90 is the p90 of tokens per byte over real provider-reported requests; zero
	// when there is too little data.
	LearnedP90         float64
	ToolOverheadTokens int
	HiddenThinking     bool
	// MaxOutput is the price entry's largest output, used when the request's max_tokens is unknown.
	MaxOutput int64
}

// Image is an input image in pixels.
type Image struct{ Width, Height int }

// Input describes one request whose usage is partly or wholly missing.
type Input struct {
	// ReportedInput and ReportedOutput are counts the provider did report (nil if not).
	ReportedInput, ReportedOutput *int64
	// RequestBytes and ResponseBytes are UTF-8 byte counts of what was sent and what was
	// received before the stream ended (-1 or 0 when unknown). No text is passed: Koinon never
	// logs or buffers prompt or response text.
	RequestBytes, ResponseBytes int64
	HasTools                    bool
	Images                      []Image
	PDFPages                    int
	// PDFPageTokens is the allowance per PDF page (0 when the entry gives none).
	PDFPageTokens int
	// Aborted is true when the response did not complete.
	Aborted bool
	// Rejected is true when the provider refused the request (a 4xx without usage): only the
	// input is charged, estimated from the request bytes, and no output.
	Rejected bool
	// MaxTokens is the request's max_tokens (0 if unknown).
	MaxTokens int64
}

// Result is the usage to record.
type Result struct {
	InputTokens, OutputTokens int64
	// Estimated is true when any count is an estimate; InputReported and OutputReported say
	// which side came from the provider.
	Estimated                     bool
	InputReported, OutputReported bool
	ThinkingUnmeasured            bool
	// MaxOutputFallback is set when output was settled at the entry's max_output because the
	// request's max_tokens was not logged.
	MaxOutputFallback bool
	// MaxOutputCapped is set when the request's max_tokens exceeded the entry's max_output.
	MaxOutputCapped             bool
	Method                      string // reported, bytes, max_tokens, max_output
	RatioStatic, RatioEffective float64
	LearnedP90TokensPerByte     float64
	OverheadTokens              int64
}

// HardMaxOutput bounds what an aborted response can be settled at when neither the request's
// max_tokens nor the entry's max_output limits it (1M tokens), and HardMaxInput bounds an input
// estimated from bytes. A request that claims max_tokens of 9e12 must not record a huge entry.
const (
	HardMaxOutput = 1_000_000
	HardMaxInput  = 10_000_000
)

// MinRatio is the smallest bytes-per-token ratio (the lower bound of pricing.ParseRatio).
const MinRatio = 0.5

// EffectiveRatio is bytes per token after calibration: calibration can only lower it (raise
// the token estimate).
func (p Params) EffectiveRatio() float64 {
	r := p.Ratio
	// The learned density is capped at the bound pricing.ParseRatio accepts for a ratio (0.5
	// bytes per token), so a degenerate sample cannot grow an estimate without limit.
	if l := min(p.LearnedP90, 1/MinRatio); l > 0 && 1/l < r {
		r = 1 / l
	}
	return r
}

// Tokens estimates bytes of text as tokens, rounding up.
func (p Params) Tokens(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return min(int64(math.Ceil(float64(bytes)/p.EffectiveRatio())), HardMaxInput)
}

// ImageTokens is the visual token count of an image: ceil(w/28) * ceil(h/28) (Anthropic vision
// docs, research S31).
func ImageTokens(i Image) int64 {
	if i.Width <= 0 || i.Height <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(i.Width)/28)) * int64(math.Ceil(float64(i.Height)/28))
}

var (
	loadOnce sync.Once
	encs     = map[string]*tiktoken.Tiktoken{}
	encMu    sync.Mutex
)

func encoding(name string) (*tiktoken.Tiktoken, error) {
	loadOnce.Do(func() { tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader()) })
	encMu.Lock()
	defer encMu.Unlock()
	if e, ok := encs[name]; ok {
		return e, nil
	}
	e, err := tiktoken.GetEncoding(name)
	if err != nil {
		return nil, fmt.Errorf("estimate: tiktoken %s: %w", name, err)
	}
	encs[name] = e
	return e, nil
}

// CountTiktoken counts text with an OpenAI encoding ("o200k_base" or "cl100k_base").
func CountTiktoken(encodingName, text string) (int64, error) {
	e, err := encoding(encodingName)
	if err != nil {
		return 0, err
	}
	return int64(len(e.EncodeOrdinary(text))), nil
}

// Estimate fills in whatever in is missing.
func Estimate(p Params, in Input) Result {
	res := Result{RatioStatic: p.Ratio, RatioEffective: p.EffectiveRatio(), LearnedP90TokensPerByte: p.LearnedP90, Method: "reported"}

	if in.ReportedInput != nil {
		res.InputTokens, res.InputReported = *in.ReportedInput, true
	} else {
		var over int64
		if in.HasTools {
			over += int64(p.ToolOverheadTokens)
		}
		for _, im := range in.Images {
			over += ImageTokens(im)
		}
		over += int64(in.PDFPages) * int64(in.PDFPageTokens)
		res.InputTokens, res.OverheadTokens, res.Estimated = p.Tokens(in.RequestBytes)+over, over, true
	}

	switch {
	case in.ReportedOutput != nil:
		res.OutputTokens, res.OutputReported = *in.ReportedOutput, true
	case in.Rejected:
		res.Estimated, res.Method = true, "bytes"
	case in.Aborted:
		// Settle at the request's max_tokens, but never above the entry's max_output (or the hard
		// ceiling when the entry has none): a lenient upstream answers a huge max_tokens with 2xx.
		bound := int64(HardMaxOutput)
		if p.MaxOutput > 0 {
			bound = min(bound, p.MaxOutput)
		}
		res.Estimated, res.Method = true, "max_tokens"
		res.OutputTokens = bound
		if in.MaxTokens > 0 {
			res.OutputTokens = min(in.MaxTokens, bound)
			res.MaxOutputCapped = in.MaxTokens > bound
		} else {
			res.MaxOutputFallback, res.Method = true, "max_output"
		}
		res.ThinkingUnmeasured = p.HiddenThinking
	default:
		res.OutputTokens, res.Estimated = p.Tokens(in.ResponseBytes), true
		res.ThinkingUnmeasured = in.Aborted && p.HiddenThinking
	}
	if res.Estimated && res.Method == "reported" {
		res.Method = "bytes"
	}
	return res
}
