// SPDX-License-Identifier: Apache-2.0

// Package contextguard decides whether an input fits the model's context (#22).
//
// One input is one message a person or an event put on the task: a webhook body, a chat paste.
// An input larger than 80% of the model's context window is not sent. Koinon keeps the full body
// as a run artifact and asks a human what to do; it never truncates an input silently. The
// window comes from the catalog (agentd resolves it); a model whose window the catalog does not
// know is assumed to have DefaultWindow, OpenCode's own assumption (ADR 0070 amendment of
// 2026-11-11).
//
// The design of capping each input and keeping its body for the agent to read in parts is adapted
// from hydro-project/infinity (Apache-2.0).
package contextguard

import "github.com/stateracloud/koinon/internal/estimate"

const (
	// DefaultWindow is the context window assumed for a model the catalog has no window for.
	DefaultWindow int64 = 200_000
	// CapPercent is the share of the window one input may take.
	CapPercent = 80
	// tokenRatio is bytes per token, the tokenizer of the Opus 4.7 and later models (about 30%
	// more tokens than older ones): the estimate over-counts for the others, which parks an
	// input a little early instead of letting it fail at the provider.
	tokenRatio = 2.3
)

// Cap is the largest input, in tokens, for a model with the given window. A window of zero or
// less is unknown and means DefaultWindow.
func Cap(window int64) int64 {
	if window <= 0 {
		window = DefaultWindow
	}
	return window * CapPercent / 100
}

// Tokens estimates the tokens of an input.
func Tokens(text string) int64 {
	return estimate.Params{Ratio: tokenRatio}.Tokens(int64(len(text)))
}

// Over is one input larger than the cap.
type Over struct {
	// Index is the position of the input in the list given to Check.
	Index  int
	Tokens int64
}

// Check returns the inputs that exceed the cap for window, in order. Inputs under the cap are
// not mentioned: they pass unchanged.
func Check(inputs []string, window int64) []Over {
	limit := Cap(window)
	var out []Over
	for i, in := range inputs {
		// A text of at most limit bytes cannot exceed limit tokens; skip the estimate.
		if int64(len(in)) <= limit {
			continue
		}
		if t := Tokens(in); t > limit {
			out = append(out, Over{Index: i, Tokens: t})
		}
	}
	return out
}
