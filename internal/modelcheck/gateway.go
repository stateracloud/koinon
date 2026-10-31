// SPDX-License-Identifier: Apache-2.0

package modelcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// KeySource yields the gateway key the check's request is sent with.
type KeySource interface {
	Key(ctx context.Context, org pgtype.UUID) (string, error)
}

// GatewayChat sends the check through agentgateway, with the org's key, so the request is subject
// to the gateway's model list, its pricing and the spend ledger like any other. It uses the route
// of the model's wire format, as a run does (ADR 0069): Messages (/v1/messages) for an anthropic
// model, chat completions (/v1/chat/completions) for the rest.
type GatewayChat struct {
	// URL is the gateway's LLM listener, for example http://koinon-gateway.koinon.svc:8080.
	URL  string
	Keys KeySource
	HTTP *http.Client
}

// Complete implements Chat.
func (g GatewayChat) Complete(ctx context.Context, org pgtype.UUID, model string, format modelformat.Format, prompt string) (string, error) {
	key, err := g.Keys.Key(ctx, org)
	if err != nil {
		return "", err
	}
	// The check sends no sampling parameters (temperature, top_p, ...): it only compares the reply
	// with the line it asked for, and newer models reject them outright (Anthropic answers 400
	// "temperature is deprecated for this model"). Keep the body to what every model accepts.
	path, payload := "/v1/chat/completions", map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 200,
		"stream":     false,
	}
	if format == modelformat.Anthropic {
		path = "/v1/messages"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if format == modelformat.Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the gateway answered %s: %s", resp.Status, snippet(raw))
	}
	if format == modelformat.Anthropic {
		return messagesText(raw)
	}
	return completionText(raw)
}

func completionText(raw []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("the reply is not a chat completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the reply has no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// messagesText joins the text blocks of a Messages reply.
func messagesText(raw []byte) (string, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("the reply is not a Messages reply: %w", err)
	}
	var sb strings.Builder
	n := 0
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
			n++
		}
	}
	if n == 0 {
		return "", errors.New("the reply has no text content")
	}
	return sb.String(), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
