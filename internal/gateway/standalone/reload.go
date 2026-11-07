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

	"github.com/stateracloud/koinon/internal/gateway"
)

// Waiter blocks until agentgateway serves a revision.
type Waiter interface {
	Wait(ctx context.Context, rev gateway.Revision) error
}

// ErrConfigRejected means agentgateway failed to load the file it was given and keeps serving
// its previous config. errors.Is matches a *ConfigRejectedError.
var ErrConfigRejected = gateway.ErrConfigRejected

// ConfigRejectedError is ErrConfigRejected with the revision and agentgateway's own reason.
type ConfigRejectedError = gateway.ConfigRejectedError

// ReloadFailure is one failed reload in agentgateway's log.
type ReloadFailure struct {
	// At is the time agentgateway logged the failure (its own clock).
	At time.Time
	// Reason is agentgateway's reload error.
	Reason string
}

// ReloadLog reads why agentgateway refused a config. agentgateway reports a failed reload only in
// its own log, never on a listener, so a deployment supplies where to read it (PodLogReload).
type ReloadLog interface {
	// LatestReloadFailure returns the latest failed reload logged strictly after the given
	// time (callers pass a lower bound; zero reads all), by agentgateway's own clock. found is false when there is none.
	LatestReloadFailure(ctx context.Context, after time.Time) (f ReloadFailure, found bool, err error)
}

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
// Polls back off: the first wait is Interval, each next one twice as long, up
// to MaxInterval. agentgateway logs every 401 as an error line, so waiting for
// a slow ConfigMap resync should not cost a line every half second.
//
// When StatsURL is set, the waiter also reads the
// agentgateway_config_synchronized gauge from GET /metrics on the stats
// listener (:15020). The gauge stays 0 after a rejection until the next file is
// loaded, and a ConfigMap delivers that file 60 to 80 s later, so a 0 alone does not say
// that the file of this revision was refused. The waiter returns ErrConfigRejected when
//
//   - the gauge reads 0 and Reload reports a reload failure logged after the newest one seen before delivery (the
//     failure is of this revision's file: it is returned at once, with agentgateway's reason), or
//   - the gauge has read 0 for RejectAfter (no Reload, or no failure in its log), with
//     RejectAfter longer than the deliverer's latency.
//
// Otherwise it waits for ctx to expire.
type ProbeWaiter struct {
	// GatewayURL is the LLM listener base, for example http://agentgateway:4000.
	GatewayURL string
	// StatsURL is the stats listener base, for example http://agentgateway:15020. Optional.
	StatsURL string
	// Interval is the wait after the first poll, default 500ms. Each later
	// wait doubles, up to MaxInterval.
	Interval time.Duration
	// MaxInterval caps the wait between polls, default 5s (or Interval, if
	// that is longer).
	MaxInterval time.Duration
	// Reload reads agentgateway's reload errors. Optional; without it a rejection is told only
	// by the gauge and RejectAfter, and carries no reason.
	Reload ReloadLog
	// RejectAfter is how long the gauge must read 0 without a log to call the config rejected,
	// default 2 minutes: longer than a ConfigMap takes to reach the pod (about a minute). A file
	// delivered at once (FileDeliverer) can use a shorter one.
	RejectAfter time.Duration
	// Client defaults to a client with a 5 s timeout.
	Client *http.Client
	// Log defaults to slog.Default.
	Log *slog.Logger
}

// defaultRejectAfter is how long the gauge must read 0 before a waiter without a reload log gives
// up. It counts time, not polls: the gauge also reads 0 from an earlier rejection until the next
// file is loaded, and a ConfigMap takes 60 to 80 s to arrive (old issue 245: four polls, 3.5 s,
// misreported the good file that followed a rejected one).
const defaultRejectAfter = 2 * time.Minute

type baselineKey struct{}

// baseline is what Prepare learned about the reload log before delivery.
type baseline struct {
	// after counts only failures logged strictly after it (the gateway's clock).
	after time.Time
	// unknown is set when the log could not be read: no failure may be attributed by the log
	// then, because an older one would pass for this file's. Only RejectAfter decides.
	unknown bool
}

// reloadLookback bounds how far back the waiter reads the gateway's log. A failed reload older
// than this is no candidate for the file being delivered, and reading less keeps a pass from
// streaming a whole rotated log file from every replica.
const reloadLookback = time.Hour

// Prepare reads agentgateway's newest reload failure before a file is delivered and remembers its
// time (or that the log could not be read, and then Wait does not attribute failures by the log), so that Wait counts only failures logged after it: a rejection can come before Wait is
// called (a file on disk is read at once), and an earlier file's failure must not count. The time
// is the gateway's own, so clock skew between the reconciler and the gateway does not matter.
// Driver calls it before Deliver when the Waiter has it.
func (w ProbeWaiter) Prepare(ctx context.Context) context.Context {
	if w.Reload == nil {
		return ctx
	}
	// Nothing logged within the lookback: the baseline is the start of the window (the
	// reconciler's clock against a window of an hour, skew is immaterial).
	b := baseline{after: time.Now().Add(-reloadLookback)}
	f, found, err := w.Reload.LatestReloadFailure(ctx, b.after)
	switch {
	case err != nil:
		b = baseline{unknown: true}
	case found:
		b.after = f.At
	}
	return context.WithValue(ctx, baselineKey{}, b)
}

// The default poll schedule: waits of 0.5 s, 1 s, 2 s, 4 s, then 5 s.
const (
	defaultInterval    = 500 * time.Millisecond
	defaultMaxInterval = 5 * time.Second
)

// probeBody is the reload probe's request.
var probeBody = []byte(`{"model":"` + ReloadProbeModel + `","messages":[{"role":"user","content":"probe"}]}`)

// Wait implements Waiter.
func (w ProbeWaiter) Wait(ctx context.Context, rev gateway.Revision) error {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	maxInterval := w.MaxInterval
	if maxInterval <= 0 {
		maxInterval = max(defaultMaxInterval, interval)
	}
	interval = min(interval, maxInterval)
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	rejectAfter := w.RejectAfter
	if rejectAfter <= 0 {
		rejectAfter = defaultRejectAfter
	}
	base, ok := ctx.Value(baselineKey{}).(baseline)
	if !ok { // no Prepare: every failure in the lookback window counts
		base = baseline{after: time.Now().Add(-reloadLookback)}
	}
	var lastErr error
	var unsyncedSince time.Time
	t := time.NewTimer(interval)
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
				if unsyncedSince.IsZero() {
					unsyncedSince = time.Now()
				}
				if w.Reload != nil && !base.unknown {
					f, found, err := w.Reload.LatestReloadFailure(ctx, base.after)
					switch {
					case err != nil:
						log.WarnContext(ctx, "cannot read agentgateway's reload errors", "err", err)
					case found:
						return &ConfigRejectedError{Revision: rev, Reason: f.Reason}
					}
				}
				if time.Since(unsyncedSince) >= rejectAfter {
					return &ConfigRejectedError{Revision: rev}
				}
			} else {
				unsyncedSince = time.Time{}
			}
		}
		// The wait starts after the probe answered, so a slow answer does not
		// eat into it (Go 1.23 timers: Reset needs no drain).
		t.Reset(interval)
		select {
		case <-ctx.Done():
			return fmt.Errorf("standalone: waiting for revision %s: %w (last: %w)", rev, ctx.Err(), lastErr)
		case <-t.C:
		}
		interval = min(2*interval, maxInterval)
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
