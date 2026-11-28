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

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
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
				// A probe waits until every live koinon-gateway copy that serves MCP acked the MCP half
				// of the State revision that holds its key and, for a new or changed server, of the
				// revision of the pass that applied it (ADR 0097 decision 2.1, #1260): before that a
				// copy can still refuse the probe key, and the refusal would be recorded as a failure.
				// The key gate decides it, as for the sandbox work and the forge credential.
				// A goroutine, not the loop: the split reads the database, and the wait for the acks takes
				// up to MCPAckWait; a copy that never acks must not stop the interval probes and the other
				// wakes (#1347).
				safego.Go(ctx, r.log, "mcp-probe", func(ctx context.Context) { r.probeDueNow(ctx, due) })
			}
		case <-t.C:
			safego.Do(r.log, "mcp-probe", func() { r.probeTick(ctx) })
		}
	}
}

// probeDueNow runs the queued probes that the key gate lets go, waits for the acks the others need,
// and then runs those that may go. A probe still held is not recorded: its key may arrive with a
// later revision (holdProbes), and the interval probe waits for the acks too. A server queued twice
// (by its pass and again from the parked set) is probed once.
func (r *Reconciler) probeDueNow(ctx context.Context, due []dueProbe) {
	seen := make(map[pgtype.UUID]bool, len(due))
	uniq := due[:0:0]
	for _, d := range due {
		if !seen[d.probe.Server] {
			seen[d.probe.Server] = true
			uniq = append(uniq, d)
		}
	}
	now, held, rev := r.newKeyGate(ctx, nil).splitProbes(ctx, uniq)
	if len(held) > 0 && rev > 0 {
		if first := now; len(first) > 0 {
			safego.Go(ctx, r.log, "mcp-probe", func(ctx context.Context) { r.probeEach(ctx, first) })
		}
		if !r.awaitGatewayAcked(ctx, rev) {
			if ctx.Err() == nil {
				r.holdProbes(held)
			}
			return
		}
		now, held, _ = r.newKeyGate(ctx, nil).splitProbes(ctx, held)
	}
	r.holdProbes(held)
	r.probeEach(ctx, now)
}

// holdProbes counts probes held back for the gateway. They are not recorded. A probe held because no
// stored revision held its key (unstored) is parked while its key is still pending, for the next pass
// that stores a revision. If such a revision was stored since the split, the pass may already have
// taken the parked set, so the probe is queued again with that revision. It is queued once: a probe
// queued with a revision is not unstored again.
func (r *Reconciler) holdProbes(held []dueProbe) {
	if len(held) == 0 {
		return
	}
	r.m.MCPProbesSkipped.Add(float64(len(held)))
	var park []pgtype.UUID
	for _, d := range held {
		if !d.unstored {
			continue
		}
		if r.keyPending(d.probe.KeyID) {
			park = append(park, d.probe.Server)
		} else if rev, ok := r.keyRev(d.probe.KeyID); ok && rev > 0 {
			r.probeSoon([]gatewayplan.Probe{d.probe}, rev)
		}
	}
	if len(park) == 0 {
		return
	}
	r.probeMu.Lock() // after keyPending: probeMu is never held while keyRevMu is taken
	defer r.probeMu.Unlock()
	if r.probeParked == nil {
		r.probeParked = map[pgtype.UUID]bool{}
	}
	for _, s := range park {
		r.probeParked[s] = true
	}
}

// takeParked returns the probes of the parked servers that the plan still probes, and empties the set.
func (r *Reconciler) takeParked() []gatewayplan.Probe {
	r.probeMu.Lock()
	parked := r.probeParked
	r.probeParked = nil
	r.probeMu.Unlock()
	if len(parked) == 0 {
		return nil
	}
	var out []gatewayplan.Probe
	for _, p := range r.plan.Probes() {
		if parked[p.Server] {
			out = append(out, p)
		}
	}
	return out
}

// ProbeAll probes every served server once.
func (r *Reconciler) ProbeAll(ctx context.Context) {
	r.probeEach(ctx, r.plan.Probes())
}

// probeTick is the interval probe. It does not start while a live copy still has to ack the newest
// stored State revision: a server whose key arrived in that revision would get a 401 that is
// recorded as a failure (#1260). A server whose probe key no stored revision holds yet (the publish
// failed) is not probed either. When the live copies cannot be read, no probe starts. The next tick
// tries again.
func (r *Reconciler) probeTick(ctx context.Context) {
	probes := r.plan.Probes()
	g := r.newKeyGate(ctx, nil)
	if g.liveErr != nil {
		r.m.MCPProbesSkipped.Add(float64(len(probes)))
		r.log.Warn("MCP interval probe skipped: the live gateway copies cannot be read", "err", g.liveErr)
		return
	}
	if latest, err := staterev.Latest(ctx, r.pool); err == nil && latest != nil && !g.ackedRev(ctx, latest.GetRevision()) {
		// Warn, not Debug: a copy whose apply keeps failing stays live and serves an old revision, so
		// this repeats every interval until the copy applies it or turns unready (#1347).
		r.m.MCPProbesSkipped.Add(float64(len(probes)))
		r.log.Warn("MCP interval probe skipped: not every gateway copy acked the newest State revision; a copy may be stuck on an old revision", "revision", latest.GetRevision())
		return
	}
	due := make([]dueProbe, len(probes))
	for i, p := range probes {
		due[i] = dueProbe{probe: p}
	}
	now, held, _ := g.splitProbes(ctx, due)
	r.holdProbes(held)
	r.probeEach(ctx, now)
}

// dueProbe is a queued probe with the State revision whose MCP half it waits for (0: none).
type dueProbe struct {
	probe gatewayplan.Probe
	rev   int64
	// unstored is set by splitProbes when the probe waits because no stored revision holds its key.
	unstored bool
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

// keyIssued records a key issued in this process: until a stored revision holds it, its agent waits
// while a live copy exists.
func (r *Reconciler) keyIssued(id string) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	if r.pendingKeys == nil {
		r.pendingKeys = map[string]bool{}
	}
	r.pendingKeys[id] = true
}

// keyPending reports whether key id was issued in this process and no stored revision holds it yet.
func (r *Reconciler) keyPending(id string) bool {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	return r.pendingKeys[id]
}

// noteKeys records, for each key that revision rev serves, the first revision that held it. A key the
// map already has keeps its earlier revision; keys rev does not serve are dropped. On the first call
// after a start the map cannot say which revision first held a key, so every key not issued in this
// process is recorded as 0: it was served before the start and never waits. A key issued in this
// process is recorded with rev, and is no longer pending. A pending key that the full State (all, with
// the keys of invalid limits) no longer holds was replaced or deleted, and is dropped.
func (r *Reconciler) noteKeys(keys []gateway.Key, rev int64, all []gateway.Key) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	first := r.keyRevs == nil
	next := make(map[string]int64, len(keys))
	for _, k := range keys {
		old, ok := r.keyRevs[k.ID]
		switch {
		case ok && old <= rev:
			next[k.ID] = old
		case first && !r.pendingKeys[k.ID]:
			next[k.ID] = 0
		default:
			next[k.ID] = rev
		}
		delete(r.pendingKeys, k.ID)
	}
	r.keyRevs = next
	inState := make(map[string]bool, len(all))
	for _, k := range all {
		inState[k.ID] = true
	}
	for id := range r.pendingKeys {
		if !inState[id] {
			delete(r.pendingKeys, id)
		}
	}
}

// keyRev is the first stored revision that served key id, if one did.
func (r *Reconciler) keyRev(id string) (int64, bool) {
	r.keyRevMu.Lock()
	defer r.keyRevMu.Unlock()
	rev, ok := r.keyRevs[id]
	return rev, ok
}

// keyGate decides which sandbox work waits until koinon-gateway serves the agent's key (D1 of
// #1239). An agent waits only when it has a key that koinon-gateway is to serve (served: its limit is
// valid) and a live copy has not acked the first revision that held that key. A key that no recorded
// revision holds (the publish failed) waits only when this process issued it (keyPending). So an agent
// that is not active (a draft keeps its key), an agent with no key (archived), a key the copies already
// serve and a key gateway.State drops never wait: a stop or a teardown is never held, and a change of
// the State for other agents holds nobody. With no live copy (the agentgateway driver) nothing waits.
// When the copy list cannot be read, every key first held by a revision stored since the start waits.
type keyGate struct {
	r       *Reconciler
	served  map[string]bool
	live    []string
	liveErr error
	acked   map[int64]bool
}

func (r *Reconciler) newKeyGate(ctx context.Context, served map[string]bool) *keyGate {
	live, err := staterev.LiveCopies(ctx, r.pool, 0, gwv1.Half_HALF_MCP)
	if err != nil {
		r.log.Warn("reading the live gateway copies failed; the work that waits for koinon-gateway (sandbox work with a key issued since the start, forge credentials, MCP probes) waits", "err", err)
	}
	return &keyGate{r: r, served: served, live: live, liveErr: err, acked: map[int64]bool{}}
}

// waits reports whether the work of w waits for koinon-gateway, and the revision it waits for (0 when
// no recorded revision holds its key, for example after a failed publish).
func (g *keyGate) waits(ctx context.Context, w *work) (bool, int64) {
	id := w.key.ID
	if id == "" || !g.served[id] || w.agent.Status != store.AgentStatusActive {
		return false, 0
	}
	return g.keyWaits(ctx, id)
}

// keyWaits reports whether key id waits until every live koinon-gateway copy serves it, and the
// revision it waits for (0 when no recorded revision holds it).
func (g *keyGate) keyWaits(ctx context.Context, id string) (bool, int64) {
	rev, ok := g.r.keyRev(id)
	if g.liveErr != nil {
		// Fail closed: without the copy list, a key issued since the start waits.
		return g.r.keyPending(id) || (ok && rev > 0), rev
	}
	if len(g.live) == 0 {
		return false, 0
	}
	if !ok {
		return g.r.keyPending(id), 0
	}
	if rev == 0 {
		return false, 0
	}
	return !g.ackedRev(ctx, rev), rev
}

// ackedRev reports whether every live koinon-gateway copy acked the MCP half of State revision rev
// (staterev.AllAcked). With no live copy it is true: nothing serves from the stream. When the copy list
// or the acks cannot be read it is false, so the work that needs them waits.
func (g *keyGate) ackedRev(ctx context.Context, rev int64) bool {
	if g.liveErr != nil {
		return false
	}
	if len(g.live) == 0 {
		return true
	}
	done, seen := g.acked[rev]
	if !seen {
		var err error
		done, err = staterev.AllAcked(ctx, g.r.pool, rev, gwv1.Half_HALF_MCP, g.live)
		if err != nil {
			g.r.log.Warn("reading the gateway acks failed; the work that needs them waits", "err", err)
			done = false
		}
		g.acked[rev] = done
	}
	return done
}

// actorAckedRev reports whether every live koinon-gateway copy acked State revision rev. The model listener
// checks the actor JWT against the actor table of its own copy (issue #1489), so a run that waits for an
// actor waits for the copies that serve only the model half too, not only the MCP copies (ackedRev). A copy
// counts when it acked either half: a copy that serves one half acks only that half, and a copy that serves
// both applies both in one step. With no live copy it is true. When the copy list or the acks cannot be
// read it is false.
func (g *keyGate) actorAckedRev(ctx context.Context, rev int64) bool {
	if !g.ackedRev(ctx, rev) {
		return false
	}
	key := -rev // the all-copies check has its own cache entry
	if done, seen := g.acked[key]; seen {
		return done
	}
	done := g.allCopiesAcked(ctx, rev)
	g.acked[key] = done
	return done
}

func (g *keyGate) allCopiesAcked(ctx context.Context, rev int64) bool {
	live, err := staterev.LiveCopies(ctx, g.r.pool, 0, gwv1.Half_HALF_MODEL)
	if err != nil {
		g.r.log.Warn("reading the live gateway copies failed; the work that needs them waits", "err", err)
		return false
	}
	for _, c := range live {
		acked := false
		for _, half := range []gwv1.Half{gwv1.Half_HALF_MCP, gwv1.Half_HALF_MODEL} {
			ok, err := staterev.AllAcked(ctx, g.r.pool, rev, half, []string{c})
			if err != nil {
				g.r.log.Warn("reading the gateway acks failed; the work that needs them waits", "err", err)
				return false
			}
			acked = acked || ok
		}
		if !acked {
			return false
		}
	}
	return true
}

// split returns the work items of idx that may go now, and those that wait with the highest revision
// they wait for.
func (g *keyGate) split(ctx context.Context, items []*work, idx []int) (goNow, wait []int, maxRev int64) {
	for _, i := range idx {
		if w, rev := g.waits(ctx, items[i]); w {
			wait = append(wait, i)
			maxRev = max(maxRev, rev)
		} else {
			goNow = append(goNow, i)
		}
	}
	return goNow, wait, maxRev
}

// splitProbes returns the probes that may start now, and those that wait with the highest revision
// they wait for. A probe waits while its key waits (keyWaits) or, when it has one, while a live copy
// has not acked the revision of the pass that applied its server. A probe whose key no recorded
// revision holds waits for no revision: only a later stored revision can bring the key. A changed
// target whose key is acked and whose pass stored no revision (the publish failed) does not wait: the
// copies then still serve the old target, so the probe can see it, but it gets no 401.
func (g *keyGate) splitProbes(ctx context.Context, due []dueProbe) (now []gatewayplan.Probe, wait []dueProbe, maxRev int64) {
	for _, d := range due {
		keyWait, keyRev := false, int64(0)
		if d.probe.KeyID != "" {
			keyWait, keyRev = g.keyWaits(ctx, d.probe.KeyID)
		}
		passWait := d.rev > 0 && !g.ackedRev(ctx, d.rev)
		if !keyWait && !passWait {
			now = append(now, d.probe)
			continue
		}
		d.unstored = keyWait && keyRev == 0
		wait = append(wait, d)
		if keyWait {
			maxRev = max(maxRev, keyRev)
		}
		if passWait {
			maxRev = max(maxRev, d.rev)
		}
	}
	return now, wait, maxRev
}

const (
	// DefaultMCPAckWait is how long the first probe of a server waits for the gateway copies to ack.
	DefaultMCPAckWait = time.Minute
	// DefaultMCPAckPoll is how often the acks are read while it waits.
	DefaultMCPAckPoll = time.Second
)

// awaitGatewayAcked waits until every live koinon-gateway copy acked the MCP half of State revision
// rev (keyGate.ackedRev), and reports whether the probe may start. It returns true at once when rev
// is 0. It returns false when ctx ends or the wait runs out; the probe is then not recorded.
func (r *Reconciler) awaitGatewayAcked(ctx context.Context, rev int64) bool {
	return r.awaitAcked(ctx, rev, func(g *keyGate) bool { return g.ackedRev(ctx, rev) })
}

// awaitActorAcked is awaitGatewayAcked for a revision that holds an actor: the model copies must ack it too.
func (r *Reconciler) awaitActorAcked(ctx context.Context, rev int64) bool {
	return r.awaitAcked(ctx, rev, func(g *keyGate) bool { return g.actorAckedRev(ctx, rev) })
}

func (r *Reconciler) awaitAcked(ctx context.Context, rev int64, acked func(*keyGate) bool) bool {
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
		// A new gate each time: the live copies change while it waits. A read that fails logs and waits.
		if acked(r.newKeyGate(ctx, nil)) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			r.log.Warn("not every gateway copy acked the MCP state; the work that needs it is held back until the next pass or interval", "revision", rev)
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
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             fmt.Sprintf("http://%s:%d/mcp", ep.Host, ep.Port),
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
