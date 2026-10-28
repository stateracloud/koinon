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
)

// KeySource yields the gateway key the check's request is sent with.
type KeySource interface {
	Key(ctx context.Context, org pgtype.UUID) (string, error)
}

// GatewayChat sends the check through agentgateway's OpenAI-compatible endpoint, with the org's
// key, so the request is subject to the gateway's model list, its pricing and the spend ledger like
// any other.
type GatewayChat struct {
	// URL is the gateway's LLM listener, for example http://koinon-gateway.koinon.svc:8080.
	URL  string
	Keys KeySource
	HTTP *http.Client
}

// Complete implements Chat.
func (g GatewayChat) Complete(ctx context.Context, org pgtype.UUID, model, prompt string) (string, error) {
	key, err := g.Keys.Key(ctx, org)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  200,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.URL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
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

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
