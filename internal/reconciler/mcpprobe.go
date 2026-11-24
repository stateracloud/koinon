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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	// mcpProbeKeyRetries is how many probes in a row koinon-gateway may refuse for the probe key before
	// the prober records the refusal (issue #1260).
	mcpProbeKeyRetries = 3
)

// errProbeKeyRefused is a probe that the gateway answered with 401: it did not know the probe key.
var errProbeKeyRefused = errors.New("the gateway refused the probe key")

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
				safego.Do(r.log, "mcp-probe", func() { r.probeEach(ctx, due) })
			}
		case <-t.C:
			safego.Do(r.log, "mcp-probe", func() { r.ProbeAll(ctx) })
		}
	}
}

// ProbeAll probes every served server once.
func (r *Reconciler) ProbeAll(ctx context.Context) {
	r.probeEach(ctx, r.plan.Probes())
}

// probeSoon queues probes for the prober to run now.
func (r *Reconciler) probeSoon(probes []gatewayplan.Probe) {
	if len(probes) == 0 {
		return
	}
	r.probeMu.Lock()
	r.probeDue = append(r.probeDue, probes...)
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
	r.probeSoon([]gatewayplan.Probe{probe})
}

func (r *Reconciler) takeProbeDue() []gatewayplan.Probe {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	due := r.probeDue
	r.probeDue = nil
	return due
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
	// refused is set when the gateway answers 401.
	refused *atomic.Bool
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := b.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && b.refused != nil {
		b.refused.Store(true)
	}
	return resp, err
}

// probeEndpoint is the gateway that serves the agents' MCP path, so the prober lists a server's tools
// as an agent would see them: koinon-gateway when it serves /mcp (gateway.mcp.driver=koinon, #1239),
// else agentgateway. agentgateway leaves out a target whose credential is sealed to the gateway key
// (ADR 0097 decision 12), so a probe through it would find no tools for that server.
func (r *Reconciler) probeEndpoint() Endpoint {
	if r.cfg.MCPGateway.Host != "" {
		return r.cfg.MCPGateway
	}
	return r.cfg.Gateway
}

// listTools lists the server's tools through the gateway, with the gateway's prefix removed.
func (r *Reconciler) listTools(ctx context.Context, p gatewayplan.Probe) ([]mcpservers.ListedTool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-mcp-prober", Version: "0"}, nil)
	ep := r.probeEndpoint()
	var refused atomic.Bool
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             fmt.Sprintf("http://%s:%d/mcp", ep.Host, ep.Port),
		HTTPClient:           &http.Client{Transport: bearerTransport{key: p.Material, base: http.DefaultTransport, refused: &refused}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		if refused.Load() {
			return nil, fmt.Errorf("connect: %w: %w", errProbeKeyRefused, err)
		}
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var out []mcpservers.ListedTool
	prefix := p.Target + "_"
	var cursor string
	for range mcpMaxToolPages {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			if refused.Load() {
				return nil, fmt.Errorf("tools/list: %w: %w", errProbeKeyRefused, err)
			}
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

func (r *Reconciler) probeOne(runCtx context.Context, p gatewayplan.Probe) {
	ctx, cancel := context.WithTimeout(runCtx, mcpProbeTimeout)
	defer cancel()
	log := r.log.With("org", p.OrgSlug, "server", p.Name)
	tools, err := r.listTools(ctx, p)
	if r.retryRefusedKey(runCtx, p, err) {
		log.Debug("koinon-gateway refused the probe key; probing again after its State poll", "err", err)
		return
	}
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
			r.resetProbeKeyRetries([]gatewayplan.Probe{p}) // the server is gone: keep no count for it
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

// retryRefusedKey reports whether a probe that koinon-gateway refused for its key is queued again
// instead of recorded. koinon-gateway learns new probe keys and targets through the State stream
// (publish, then NOTIFY or its poll), but the prober runs right after the pass applied them, so its first
// probe after a restart or for a new server can come before the gateway has them. Such a refusal is not
// the server's fault: up to mcpProbeKeyRetries refusals in a row are queued again after
// MCPProbeKeyRetryAfter and not recorded; the refusals after them are recorded. Any other outcome resets
// the count. ctx is the prober's run context, not the probe's own timeout. agentgateway (MCPGateway
// unset) gets its keys with the config that the pass applied, so its refusals are recorded at once.
//
// Interim, issue #1260: exit when the reconciler probes only after every live koinon-gateway copy acked
// the MCP half of the revision (staterev.AllAcked, ADR 0097 decision 2.1); then remove this retry.
func (r *Reconciler) retryRefusedKey(ctx context.Context, p gatewayplan.Probe, err error) bool {
	refused := r.cfg.MCPGateway.Host != "" && errors.Is(err, errProbeKeyRefused)
	r.probeMu.Lock()
	if !refused {
		delete(r.probeKeyRetries, p.Server)
		r.probeMu.Unlock()
		return false
	}
	if r.probeKeyRetries[p.Server] >= mcpProbeKeyRetries {
		// The count stays at the cap, so later refusals are recorded at once until another outcome.
		r.probeMu.Unlock()
		return false
	}
	if r.probeKeyRetries == nil {
		r.probeKeyRetries = map[pgtype.UUID]int{}
	}
	r.probeKeyRetries[p.Server]++
	r.probeMu.Unlock()
	time.AfterFunc(r.cfg.MCPProbeKeyRetryAfter, func() {
		if ctx.Err() == nil {
			r.probeSoon([]gatewayplan.Probe{p})
		}
	})
	return true
}

// resetProbeKeyRetries forgets the refusal counts of the given servers (issue #1260).
func (r *Reconciler) resetProbeKeyRetries(probes []gatewayplan.Probe) {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	for _, p := range probes {
		delete(r.probeKeyRetries, p.Server)
	}
}
