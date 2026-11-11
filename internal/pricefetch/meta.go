// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ModelMeta is what OpenRouter's model list says about one model apart from its price (ADR 0070,
// amendment of 2026-11-11). The parser that reads the prices reads these fields in the same pass:
// there is one OpenRouter parser.
//
// Display fields (everything but the two limits) take effect when stored. ContextLength and
// MaxOutput set compaction thresholds, so they only become proposals (ADR 0070 decision 8).
type ModelMeta struct {
	Model       string `json:"model"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Created is OpenRouter's unix time for the model; 0 when it gives none.
	Created             int64    `json:"created,omitempty"`
	InputModalities     []string `json:"inputModalities,omitempty"`
	OutputModalities    []string `json:"outputModalities,omitempty"`
	Tokenizer           string   `json:"tokenizer,omitempty"`
	SupportedParameters []string `json:"supportedParameters,omitempty"`
	// ContextLength and MaxOutput are in tokens; 0 means the list gave none.
	ContextLength int `json:"contextLength,omitempty"`
	MaxOutput     int `json:"maxOutput,omitempty"`
}

// Bounds of the stored display strings and lists, so one odd model cannot make a row large.
const (
	maxNameLen        = 300
	maxDescriptionLen = 2000
	maxListItems      = 64
	maxItemLen        = 64
	maxTokens         = 10_000_000
)

// metaOf reads the metadata of one listed model. A field that is not what it should be is left
// empty (the display falls back, the limit proposes nothing); it never fails the fetch.
func metaOf(m orModel) ModelMeta {
	out := ModelMeta{
		Model:               m.ID,
		Name:                clip(m.Name, maxNameLen),
		Description:         clip(m.Description, maxDescriptionLen),
		Created:             int64Of(m.Created, 1<<40),
		SupportedParameters: cleanList(m.SupportedParameters),
		ContextLength:       tokensOf(m.ContextLength),
	}
	if m.Architecture != nil {
		out.InputModalities = cleanList(m.Architecture.InputModalities)
		out.OutputModalities = cleanList(m.Architecture.OutputModalities)
		out.Tokenizer = clip(m.Architecture.Tokenizer, maxItemLen)
	}
	if m.TopProvider != nil {
		out.MaxOutput = tokensOf(m.TopProvider.MaxCompletionTokens)
		if out.ContextLength == 0 {
			out.ContextLength = tokensOf(m.TopProvider.ContextLength)
		}
	}
	return out
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// cleanList keeps up to maxListItems non-empty, clipped, distinct items in their order.
func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		s = clip(s, maxItemLen)
		if s == "" || slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
		if len(out) == maxListItems {
			break
		}
	}
	return out
}

// int64Of reads a JSON integer in [0, limit]; anything else is 0.
func int64Of(raw json.RawMessage, limit int64) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > limit {
		return 0
	}
	return n
}

func tokensOf(raw json.RawMessage) int { return int(int64Of(raw, maxTokens)) }

// EndpointsFetcher is the optional part of an adapter that reads per-model endpoint data
// (OpenRouter's GET /api/v1/models/{author}/{slug}/endpoints).
type EndpointsFetcher interface {
	FetchEndpoints(ctx context.Context, model string, now time.Time) (ModelEndpoints, error)
}

// Endpoint is one upstream that serves a model, as far as Koinon shows it. Its prices are never
// read: per-upstream prices are not imported (ADR 0098 decision 2.7).
type Endpoint struct {
	Provider                string   `json:"provider,omitempty"`
	Tag                     string   `json:"tag,omitempty"`
	Quantization            string   `json:"quantization,omitempty"`
	ContextLength           int      `json:"contextLength,omitempty"`
	MaxCompletionTokens     int      `json:"maxCompletionTokens,omitempty"`
	MaxPromptTokens         int      `json:"maxPromptTokens,omitempty"`
	SupportsImplicitCaching bool     `json:"supportsImplicitCaching,omitempty"`
	SupportedParameters     []string `json:"supportedParameters,omitempty"`
}

// ModelEndpoints is the upstream list of one model at a time.
type ModelEndpoints struct {
	Model     string     `json:"model"`
	FetchedAt time.Time  `json:"fetchedAt"`
	Endpoints []Endpoint `json:"endpoints"`
}

// MaxEndpoints bounds how many upstreams are kept per model (DeepSeek V4.1 Flash has 41).
const MaxEndpoints = 100

type orEndpoints struct {
	Data *struct {
		Endpoints []struct {
			ProviderName            string          `json:"provider_name"`
			Tag                     string          `json:"tag"`
			Quantization            string          `json:"quantization"`
			ContextLength           json.RawMessage `json:"context_length"`
			MaxCompletionTokens     json.RawMessage `json:"max_completion_tokens"`
			MaxPromptTokens         json.RawMessage `json:"max_prompt_tokens"`
			SupportsImplicitCaching bool            `json:"supports_implicit_caching"`
			SupportedParameters     []string        `json:"supported_parameters"`
		} `json:"endpoints"`
	} `json:"data"`
}

// ParseOpenRouterEndpoints decodes an endpoints route body for model.
func ParseOpenRouterEndpoints(model string, body []byte, now time.Time) (ModelEndpoints, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	var l orEndpoints
	if err := d.Decode(&l); err != nil {
		return ModelEndpoints{}, fmt.Errorf("pricefetch: openrouter endpoints: decode: %w", err)
	}
	if d.More() {
		return ModelEndpoints{}, errors.New("pricefetch: openrouter endpoints: trailing data")
	}
	if l.Data == nil {
		return ModelEndpoints{}, errors.New("pricefetch: openrouter endpoints: the body has no data object")
	}
	out := ModelEndpoints{Model: model, FetchedAt: now.UTC()}
	for _, e := range l.Data.Endpoints {
		if len(out.Endpoints) == MaxEndpoints {
			break
		}
		ep := Endpoint{
			Provider:                clip(e.ProviderName, maxItemLen),
			Tag:                     clip(e.Tag, maxItemLen),
			Quantization:            clip(e.Quantization, maxItemLen),
			ContextLength:           tokensOf(e.ContextLength),
			MaxCompletionTokens:     tokensOf(e.MaxCompletionTokens),
			MaxPromptTokens:         tokensOf(e.MaxPromptTokens),
			SupportsImplicitCaching: e.SupportsImplicitCaching,
			SupportedParameters:     cleanList(e.SupportedParameters),
		}
		out.Endpoints = append(out.Endpoints, ep)
	}
	return out, nil
}
