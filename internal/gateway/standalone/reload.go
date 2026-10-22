// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// AdminWaiter detects a reload by polling agentgateway's admin endpoint
// GET /config_dump until the dump contains the revision marker that Render
// puts in frontendPolicies.accessLog.add[MarkerField].
//
// agentgateway watches its config file and reloads every section except the
// top-level `config` (agentgateway docs, standalone configuration overview);
// a ConfigMap update additionally waits for the kubelet to resync the volume
// (11 s in the spike, up to about a minute by default). The admin listener
// binds to localhost unless Options.AdminAddr opens it.
//
// When StatsURL is set, the waiter also reads the
// agentgateway_config_synchronized gauge from GET /metrics on the stats
// listener (:15020) and returns ErrConfigRejected once it reads 0 on
// consecutive polls, instead of waiting for ctx to expire.
type AdminWaiter struct {
	// AdminURL is the admin listener base, for example http://agentgateway:15000.
	AdminURL string
	// StatsURL is the stats listener base, for example http://agentgateway:15020. Optional.
	StatsURL string
	// Interval between polls, default 500ms.
	Interval time.Duration
	// Client defaults to a client with a 5 s timeout.
	Client *http.Client
}

// rejectAfter is how many consecutive polls must see the gauge at 0 before
// Wait gives up. A single 0 can be the previous, already rejected file.
const rejectAfter = 4

// Wait implements Waiter.
func (w AdminWaiter) Wait(ctx context.Context, rev gateway.Revision) error {
	interval := w.Interval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	marker := []byte(string(rev))
	var lastErr error
	unsynced := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		body, err := get(ctx, client, strings.TrimRight(w.AdminURL, "/")+"/config_dump")
		switch {
		case err != nil:
			lastErr = err
		case bytes.Contains(body, marker):
			return nil
		default:
			lastErr = fmt.Errorf("revision %s not live yet", rev)
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

func (w AdminWaiter) synchronized(ctx context.Context, client *http.Client) (bool, error) {
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
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
