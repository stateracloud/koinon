// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/procoduck/koinon/internal/gateway"
)

// Waiter blocks until agentgateway serves a revision.
type Waiter interface {
	Wait(ctx context.Context, rev gateway.Revision) error
}

// ErrConfigRejected means agentgateway failed to load the file it was given
// and keeps serving its previous config.
var ErrConfigRejected = errors.New("standalone: agentgateway rejected the config")

// ProbeWaiter detects a reload on agentgateway's data plane: it sends a chat
// completion with the revision's probe key (ReloadProbeKey) to the LLM
// listener until agentgateway stops answering 401. Only the config of that
// revision accepts the key; the probe itself never reaches a model, because
// the key's zero budget (429) or the llm authorization rule (403) refuses it
// before routing, and it asks for a model no config serves (404).
//
// agentgateway watches its config file and reloads every section except the
// top-level `config` (agentgateway docs, standalone configuration overview);
// a ConfigMap update additionally waits for the kubelet to resync the volume
// (11 s in the spike, up to about a minute by default).
//
// When StatsURL is set, the waiter also reads the
// agentgateway_config_synchronized gauge from GET /metrics on the stats
// listener (:15020) and returns ErrConfigRejected once it reads 0 on
// consecutive polls, instead of waiting for ctx to expire.
type ProbeWaiter struct {
	// GatewayURL is the LLM listener base, for example http://agentgateway:4000.
	GatewayURL string
	// StatsURL is the stats listener base, for example http://agentgateway:15020. Optional.
	StatsURL string
	// Interval between polls, default 500ms.
	Interval time.Duration
	// Client defaults to a client with a 5 s timeout.
	Client *http.Client
	// Log defaults to slog.Default.
	Log *slog.Logger
}

// rejectAfter is how many consecutive polls must see the gauge at 0 before
// Wait gives up. A single 0 can be the previous, already rejected file.
const rejectAfter = 4

// probeBody is the reload probe's request.
var probeBody = []byte(`{"model":"` + ReloadProbeModel + `","messages":[{"role":"user","content":"probe"}]}`)

// Wait implements Waiter.
func (w ProbeWaiter) Wait(ctx context.Context, rev gateway.Revision) error {
	interval := w.Interval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	var lastErr error
	unsynced := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		status, err := w.probe(ctx, client, rev)
		switch {
		case err != nil:
			// A probe cut short by ctx says nothing; keep the previous answer.
			if ctx.Err() == nil || lastErr == nil {
				lastErr = err
			}
		case status == http.StatusUnauthorized:
			lastErr = fmt.Errorf("revision %s not live yet", rev)
		default:
			log.DebugContext(ctx, "agentgateway serves the revision", "revision", string(rev), "probe_status", status)
			return nil
		}
		if w.StatsURL != "" {
			if synced, err := w.synchronized(ctx, client); err == nil && !synced {
				unsynced++
				if unsynced >= rejectAfter {
					return fmt.Errorf("%w (revision %s)", ErrConfigRejected, rev)
				}
			} else {
				unsynced = 0
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("standalone: waiting for revision %s: %w (last: %w)", rev, ctx.Err(), lastErr)
		case <-t.C:
		}
	}
}

// probe sends the probe request for rev and returns the response status.
func (w ProbeWaiter) probe(ctx context.Context, client *http.Client, rev gateway.Revision) (int, error) {
	url := strings.TrimRight(w.GatewayURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(probeBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ReloadProbeKey(rev))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

func (w ProbeWaiter) synchronized(ctx context.Context, client *http.Client) (bool, error) {
	body, err := get(ctx, client, strings.TrimRight(w.StatsURL, "/")+"/metrics")
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "agentgateway_config_synchronized "); ok {
			return strings.TrimSpace(v) != "0", nil
		}
	}
	return false, errors.New("agentgateway_config_synchronized not found")
}

func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
