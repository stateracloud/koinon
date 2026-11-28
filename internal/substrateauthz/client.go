// SPDX-License-Identifier: Apache-2.0

package substrateauthz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client asks the provisioner for an org atespace. It implements the substrate driver's Provisioner.
type Client struct {
	// URL is the provisioner's base URL, for example http://koinon-provisioner.koinon.svc:8086.
	URL string
	// TokenFile holds a projected ServiceAccount token whose audience is the provisioner's. It is read again on
	// every call, so a rotated token is picked up.
	TokenFile string
	HTTP      *http.Client
}

// EnsureAtespace implements the substrate driver's Provisioner. Any error is for the caller to retry.
func (c *Client) EnsureAtespace(ctx context.Context, name string) error {
	tok, err := os.ReadFile(c.TokenFile) //nolint:gosec // the path is the operator's flag, a projected token
	if err != nil {
		return fmt.Errorf("read the provisioner token: %w", err)
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+AtespacesPath, bytes.NewReader(body)) //nolint:gosec // the URL is the operator's flag, the in-cluster provisioner
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req) //nolint:gosec // see the request above
	if err != nil {
		return fmt.Errorf("ask the provisioner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("the provisioner answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}
