// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
	"github.com/stateracloud/koinon/internal/safego"
)

// The prober (plan item MC1). Every MCP probe interval it connects to the gateway with each served
// server's probe key, as an agent would, runs initialize and tools/list, and records the result:
//
//   - tools came back: the server is healthy, and the list is synced into mcp_tools (new tools enter
//     the ask tier, changed ones are flagged for review);
//   - nothing came back, or the gateway refused: a failure. Three in a row make the server unhealthy
//     with a reason (its tools drop out of every agent's grants on the next pass); one success makes
//     it healthy again.
//
// The gateway runs with failureMode failOpen, so a target it cannot reach is skipped without an error
// and its tools are simply missing from the list; "no tools" is therefore the failure the prober
// sees. The reason is only what the gateway returned: the credential never leaves the gateway, so the
// prober does not ask the server itself.

const (
	mcpProbeTimeout    = 20 * time.Second
	mcpProbeParallel   = 8
	mcpMaxToolPages    = 20
	reasonNoTools      = "the gateway got no tools from the server: it is unreachable, rejected the credential or lists none (the gateway does not say which)"
	reasonGatewayError = "the gateway refused or failed the probe"
	// mcpProbeWakeEvery is the shortest time between two probes of one server that gateway wakes
	// (#1173) start. A wake inside it is held back to its end, not dropped: the text the gateway saw
	// may have changed after the last probe started. Any role may notify the channel, so this bound
	// also holds when many gateway replicas, or a misbehaving client, send wakes.
	mcpProbeWakeEvery = 5 * time.Second
)

// RunProber probes until ctx ends. It is started by Run and only has work on the replica that
// leads.
func (r *Reconciler) RunProber(ctx context.Context) {
	t := time.NewTicker(r.cfg.MCPProbeInterval)
	defer t.Stop()
	wake := r.probeWakeChan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			// A pass applied servers that were not served before (or with another URL or credential):
			// list them now, so a new server turns healthy in seconds instead of at the next tick.
			if due := r.takeProbeDue(); len(due) > 0 {
				// A wake (rev 0) needs no ack: it probes a server that is already served. The first
				// probe of a new or changed server waits until every live koinon-gateway copy that
				// serves MCP acked the MCP half of the State revision that holds it (ADR 0097
				// decision 2.1, #1260): before that a copy can still refuse the probe key, and the
				// refusal would be recorded as a failure.
				var now, held []gatewayplan.Probe
				var rev int64
				for _, d := range due {
					if d.rev == 0 {
						now = append(now, d.probe)
					} else {
						held = append(held, d.probe)
						rev = max(rev, d.rev)
					}
				}
				if len(now) > 0 {
					safego.Do(r.log, "mcp-probe", func() { r.probeEach(ctx, now) })
				}
				if len(held) > 0 {
					safego.Do(r.log, "mcp-probe", func() {
						if r.awaitGatewayAcked(ctx, rev) {
							r.probeEach(ctx, held)
						} // else held back, not recorded; the interval probe waits for the ack too
					})
				}
			}
		case <-t.C:
			safego.Do(r.log, "mcp-probe", func() { r.probeTick(ctx) })
		}
	}
}

// ProbeAll probes every served server once.
func (r *Reconciler) ProbeAll(ctx context.Context) {
	r.probeEach(ctx, r.plan.Probes())
}

// probeTick is the interval probe. It does not start while a live copy still has to ack the newest
// stored State revision: a server whose key arrived in that revision would get a 401 that is
// recorded as a failure (#1260). The next tick tries again.
func (r *Reconciler) probeTick(ctx context.Context) {
	if latest, err := staterev.Latest(ctx, r.pool); err == nil && latest != nil {
		if ok, err := r.gatewayAckedNow(ctx, latest.GetRevision()); err == nil && !ok {
			r.log.Debug("MCP interval probe skipped: not every gateway copy acked the newest State revision", "revision", latest.GetRevision())
			return
		}
	}
	r.ProbeAll(ctx)
}

// dueProbe is a queued probe with the State revision whose MCP half it waits for (0: none).
type dueProbe struct {
	probe gatewayplan.Probe
	rev   int64
}

// probeSoon queues probes for the prober to run now, after every live koinon-gateway copy acked the
// MCP half of State revision rev (0: do not wait).
func (r *Reconciler) probeSoon(probes []gatewayplan.Probe, rev int64) {
	if len(probes) == 0 {
		return
	}
	r.probeMu.Lock()
	for _, p := range probes {
		r.probeDue = append(r.probeDue, dueProbe{probe: p, rev: rev})
	}
	r.probeMu.Unlock()
	select {
	case r.probeWakeChan() <- struct{}{}:
	default: // a wake is already pending
	}
}

// wakeProbe handles a gateway wake (#1173): the gateway served a list whose text for a tool differs
// from the text the prober recorded, so the server is probed now, not at the next interval. That sync
// flags the tool for review, and the console shows its new text. At most one probe per server starts
// per mcpProbeWakeEvery. A payload that names no served server is ignored.
func (r *Reconciler) wakeProbe(ctx context.Context, payload string) {
	org, server, ok := mcpservers.ParseProbeWakePayload(payload)
	if !ok {
		// Debug, not Warn: any role that can NOTIFY could fill the log with forged wakes.
		r.log.Debug("mcp prober: ignoring a wake with a bad payload")
		return
	}
	// Look the server up in the plan first, so a wake for a server the plan does not probe (a forged
	// payload, or a server deleted since) leaves no state behind.
	var probe gatewayplan.Probe
	found := false
	for _, p := range r.plan.Probes() {
		if p.Org == org && p.Server == server {
			probe, found = p, true
			break
		}
	}
	if !found {
		return
	}
	now := time.Now()
	r.probeMu.Lock()
	if r.probeHeld[server] {
		r.probeMu.Unlock()
		return // a held wake already covers this one
	}
	if last, seen := r.probeAsked[server]; seen && now.Sub(last) < mcpProbeWakeEvery {
		if r.probeHeld == nil {
			r.probeHeld = map[pgtype.UUID]bool{}
		}
		r.probeHeld[server] = true
		r.probeMu.Unlock()
		time.AfterFunc(mcpProbeWakeEvery-now.Sub(last), func() {
			r.probeMu.Lock()
			delete(r.probeHeld, server)
			r.probeMu.Unlock()
			if ctx.Err() == nil {
				r.wakeProbe(ctx, payload)
			}
		})
		return
	}
	if r.probeAsked == nil {
		r.probeAsked = map[pgtype.UUID]time.Time{}
	}
	for k, t := range r.probeAsked {
		if now.Sub(t) >= mcpProbeWakeEvery {
			delete(r.probeAsked, k)
		}
	}
	r.probeAsked[server] = now
	r.probeMu.Unlock()
	r.probeSoon([]gatewayplan.Probe{probe}, 0)
}

func (r *Reconciler) takeProbeDue() []dueProbe {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	due := r.probeDue
	r.probeDue = nil
	return due
}

// gatewayAckedNow reports whether every live gateway copy that serves MCP acked the MCP half of rev.
// No such copy counts as acked: nothing registered one (another gateway driver, or the gateway is
// down), and a down gateway must still show as a failed probe.
func (r *Reconciler) gatewayAckedNow(ctx context.Context, rev int64) (bool, error) {
	live, err := staterev.LiveCopies(ctx, r.pool, 0, gwv1.Half_HALF_MCP)
	if err != nil {
		return false, err
	}
	if len(live) == 0 {
		return true, nil
	}
	return staterev.AllAcked(ctx, r.pool, rev, gwv1.Half_HALF_MCP, live)
}

const (
	// DefaultMCPAckWait is how long the first probe of a server waits for the gateway copies to ack.
	DefaultMCPAckWait = time.Minute
	// DefaultMCPAckPoll is how often the acks are read while it waits.
	DefaultMCPAckPoll = time.Second
)

// awaitGatewayAcked waits until every live koinon-gateway copy acked the MCP half of State revision
// rev (staterev.AllAcked), and reports whether the probe may start (gatewayAckedNow says when none
// has to ack). It returns true at once when rev is 0. It returns false when ctx ends or the wait runs out; the probe is then not recorded.
func (r *Reconciler) awaitGatewayAcked(ctx context.Context, rev int64) bool {
	if rev <= 0 {
		return true
	}
	wait, poll := r.cfg.MCPAckWait, r.cfg.MCPAckPoll
	if wait <= 0 {
		wait = DefaultMCPAckWait
	}
	if poll <= 0 {
		poll = DefaultMCPAckPoll
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if ok, err := r.gatewayAckedNow(ctx, rev); err != nil {
			r.log.Warn("reading the gateway acks failed; the MCP probe waits", "err", err)
		} else if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			r.log.Warn("not every gateway copy acked the MCP state; the MCP probe is held back until the next interval", "revision", rev)
			return false
		case <-time.After(poll):
		}
	}
}

func (r *Reconciler) probeWakeChan() chan struct{} {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeWake == nil {
		r.probeWake = make(chan struct{}, 1)
	}
	return r.probeWake
}

// probeEach probes the given servers, a few at a time.
func (r *Reconciler) probeEach(ctx context.Context, probes []gatewayplan.Probe) {
	sem := make(chan struct{}, mcpProbeParallel)
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		sem <- struct{}{}
		safego.Go(ctx, r.log, "mcp-probe", func(ctx context.Context) {
			defer wg.Done()
			defer func() { <-sem }()
			r.probeOne(ctx, p)
		})
	}
	wg.Wait()
}

type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(req)
}

// listTools lists the server's tools through the gateway, with the gateway's prefix removed.
func (r *Reconciler) listTools(ctx context.Context, p gatewayplan.Probe) ([]mcpservers.ListedTool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-mcp-prober", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             fmt.Sprintf("http://%s:%d/mcp", r.cfg.Gateway.Host, r.cfg.Gateway.Port),
		HTTPClient:           &http.Client{Transport: bearerTransport{key: p.Material, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var out []mcpservers.ListedTool
	prefix := p.Target + "_"
	var cursor string
	for range mcpMaxToolPages {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		for _, t := range res.Tools {
			lt := mcpservers.ListedTool{Name: strings.TrimPrefix(t.Name, prefix), Description: t.Description}
			if raw, err := jsonOf(t.InputSchema); err == nil {
				lt.InputSchema = raw
			}
			if t.Annotations != nil {
				lt.ReadOnlyHint = t.Annotations.ReadOnlyHint
				if raw, err := jsonOf(t.Annotations); err == nil {
					lt.Annotations = raw
				}
			}
			lt.Title = t.Title
			if t.OutputSchema != nil {
				if raw, err := jsonOf(t.OutputSchema); err == nil {
					lt.OutputSchema = raw
				}
			}
			out = append(out, lt)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

func jsonOf(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return b, err
}

func (r *Reconciler) probeOne(ctx context.Context, p gatewayplan.Probe) {
	ctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout)
	defer cancel()
	log := r.log.With("org", p.OrgSlug, "server", p.Name)
	tools, err := r.listTools(ctx, p)
	reason := ""
	switch {
	case err != nil:
		log.Debug("MCP probe failed", "err", err)
		reason = reasonGatewayError
	case len(tools) == 0:
		reason = reasonNoTools
	}
	if reason != "" {
		// Only what the gateway returned: the server's credential never leaves the gateway, so the
		// prober cannot ask the server itself why.
		if _, changed, err := r.mcp.RecordProbe(ctx, p.Org, p.Server, false, reason, false); errors.Is(err, mcpservers.ErrNotFound) {
			return
		} else if err != nil {
			log.Warn("recording an MCP probe failed", "err", err)
		} else if changed {
			log.Warn("MCP server is unhealthy; its tools are withdrawn from agents", "reason", reason)
		}
		return
	}
	res, err := r.mcp.SyncTools(ctx, p.Org, p.Server, tools)
	if err != nil {
		log.Warn("syncing MCP tools failed", "err", err)
		return
	}
	if len(res.New)+len(res.Changed) > 0 {
		log.Info("MCP tools synced", "new", len(res.New), "changed", len(res.Changed), "absent", len(res.Absent))
	}
	if _, changed, err := r.mcp.RecordProbe(ctx, p.Org, p.Server, true, "", true); err != nil {
		log.Warn("recording an MCP probe failed", "err", err)
	} else if changed {
		log.Info("MCP server is healthy")
	}
}
