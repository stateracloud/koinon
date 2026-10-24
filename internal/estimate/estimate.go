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
//  3. An aborted request whose model bills hidden thinking cannot be measured from the visible
//     text: the entry is flagged ThinkingUnmeasured and its output is settled at max_tokens.
//  4. For OpenAI-family models the exact tiktoken encoding counts the text when the text is
//     available. The BPE files are compiled into the binary (github.com/pkoukk/tiktoken-go-loader,
//     MIT); nothing is downloaded at run time.
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

	"github.com/stateracloud/koinon/pricing"
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
}

// Image is an input image in pixels.
type Image struct{ Width, Height int }

// Input describes one request whose usage is partly or wholly missing.
type Input struct {
	// ReportedInput and ReportedOutput are counts the provider did report (nil if not).
	ReportedInput, ReportedOutput *int64
	// RequestBytes and ResponseBytes are UTF-8 byte counts of what was sent and what was
	// received before the stream ended. RequestText and ResponseText, when present, are
	// counted exactly with tiktoken for OpenAI-family models.
	RequestBytes, ResponseBytes int64
	RequestText, ResponseText   string
	HasTools                    bool
	Images                      []Image
	PDFPages                    int
	// PDFPageTokens is the allowance per PDF page (0 when the entry gives none).
	PDFPageTokens int
	// Aborted is true when the response did not complete.
	Aborted bool
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
	Method                        string // reported, bytes, tiktoken, max_tokens
	RatioStatic, RatioEffective   float64
	LearnedP90TokensPerByte       float64
	OverheadTokens                int64
}

// EffectiveRatio is bytes per token after calibration: calibration can only lower it (raise
// the token estimate).
func (p Params) EffectiveRatio() float64 {
	r := p.Ratio
	if p.LearnedP90 > 0 && 1/p.LearnedP90 < r {
		r = 1 / p.LearnedP90
	}
	return r
}

// Tokens estimates bytes of text as tokens, rounding up.
func (p Params) Tokens(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(bytes) / p.EffectiveRatio()))
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

func (p Params) encodingName() string {
	switch p.Tokenizer {
	case pricing.TokenizerO200K:
		return "o200k_base"
	case pricing.TokenizerCL100K:
		return "cl100k_base"
	}
	return ""
}

func (p Params) count(bytes int64, text string) (n int64, exact bool) {
	if name := p.encodingName(); name != "" && text != "" {
		if n, err := CountTiktoken(name, text); err == nil {
			return n, true
		}
	}
	return p.Tokens(bytes), false
}

// Estimate fills in whatever in is missing.
func Estimate(p Params, in Input) Result {
	res := Result{RatioStatic: p.Ratio, RatioEffective: p.EffectiveRatio(), LearnedP90TokensPerByte: p.LearnedP90, Method: "reported"}
	exact := true

	if in.ReportedInput != nil {
		res.InputTokens, res.InputReported = *in.ReportedInput, true
	} else {
		n, ex := p.count(in.RequestBytes, in.RequestText)
		var over int64
		if in.HasTools {
			over += int64(p.ToolOverheadTokens)
		}
		for _, im := range in.Images {
			over += ImageTokens(im)
		}
		over += int64(in.PDFPages) * int64(in.PDFPageTokens)
		res.InputTokens, res.OverheadTokens = n+over, over
		res.Estimated, exact = true, exact && ex
	}

	switch {
	case in.ReportedOutput != nil:
		res.OutputTokens, res.OutputReported = *in.ReportedOutput, true
	case in.Aborted && p.HiddenThinking && in.MaxTokens > 0:
		// Visible text says nothing about billed thinking: settle at the most the request
		// could have produced.
		res.OutputTokens, res.ThinkingUnmeasured, res.Estimated, exact = in.MaxTokens, true, true, false
		res.Method = "max_tokens"
	default:
		n, ex := p.count(in.ResponseBytes, in.ResponseText)
		res.OutputTokens = n
		res.Estimated, exact = true, exact && ex
		if in.Aborted && p.HiddenThinking {
			res.ThinkingUnmeasured = true // visible estimate only: max_tokens was not known
		}
	}
	if res.Estimated && res.Method == "reported" {
		res.Method = "bytes"
		if exact {
			res.Method = "tiktoken"
		}
	}
	return res
}
