// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// gate is the driver's record of one actor's guest traffic. Any request through the router resumes
// a suspended actor (ADR 0119, Verification), so before the driver suspends or deletes an actor it
// pauses the gate: no new router call starts until the gate opens and the control API says the
// actor runs, and the output streams the driver holds to the guest are cancelled. A suspend then
// drains the calls that started before the pause (see drain) and verifies the state afterwards,
// because a request that was already on its way can still reach the router late.
type gate struct {
	mu     sync.Mutex
	paused int
	wake   chan struct{} // closed and replaced whenever paused changes
	procs  map[*proc]struct{}
	next   uint64
	active map[uint64]call // every router call in flight, until its end runs
	idle   chan struct{}   // closed when active becomes empty; nil while nobody waits for that
	dead   bool            // dropped from the driver's map: users fetch the gate again
}

// callKind says how a pause treats a router call that is in flight.
type callKind int

const (
	// shortCall is a call that ends by itself (a stdin write, an upload, a process start or signal):
	// a suspend waits for it, so that its work is not lost, and cancels it only after DrainTimeout.
	shortCall callKind = iota
	// streamCall is an output stream that ends only with the process: a pause cancels it at once.
	streamCall
)

type call struct {
	cancel context.CancelFunc
	kind   callKind
}

func newGate() *gate {
	return &gate{wake: make(chan struct{}), procs: map[*proc]struct{}{}, active: map[uint64]call{}}
}

func (d *Driver) gate(k key) *gate {
	d.mu.Lock()
	defer d.mu.Unlock()
	g, ok := d.gates[k]
	if !ok {
		g = newGate()
		d.gates[k] = g
	}
	return g
}

// forget drops the gate of an actor once nothing uses it: no process, no pause, no call in flight.
// The gate is marked dead under its own lock, so a caller that fetched it just before must fetch
// again and cannot register on a gate that is no longer in the map. A gate is cheap to recreate,
// so it is dropped whenever it is idle, for an actor that is gone and for one that is not.
func (d *Driver) forget(k key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	g, ok := d.gates[k]
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.procs) == 0 && g.paused == 0 && len(g.active) == 0 {
		g.dead = true
		delete(d.gates, k)
	}
}

// pauseGate closes the gate of k and returns it, for the caller to unpause.
func (d *Driver) pauseGate(k key) *gate {
	for {
		g := d.gate(k)
		if g.pause() {
			return g
		}
	}
}

// pause closes the gate and cancels the output streams in flight. Short calls go on: drain waits
// for them. Calls nest. It reports false when the gate was dropped meanwhile.
func (g *gate) pause() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return false
	}
	g.paused++
	for _, c := range g.active {
		if c.kind == streamCall {
			c.cancel()
		}
	}
	g.broadcast()
	return true
}

// cancelAll cancels every router call in flight. Each stays registered until its end runs.
func (g *gate) cancelAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.active {
		c.cancel()
	}
}

// begin registers one router call. The check of the gate and the registration happen under one
// lock, so a pause either sees the call (and cancels or drains it), or the call sees the pause and
// does not start: no call can slip between the two. The returned end must be called once the call
// is over: the client side of it has returned.
func (g *gate) begin(parent context.Context, kind callKind) (ctx context.Context, end func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused > 0 || g.dead {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	g.next++
	id := g.next
	g.active[id] = call{cancel: cancel, kind: kind}
	return ctx, func() {
		g.mu.Lock()
		delete(g.active, id)
		if len(g.active) == 0 && g.idle != nil {
			close(g.idle)
			g.idle = nil
		}
		g.mu.Unlock()
		cancel()
	}, true
}

// idleChan returns a channel that is closed once no router call is in flight, or nil when none is.
func (g *gate) idleChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.active) == 0 {
		return nil
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	return g.idle
}

// drain waits, on a paused gate, until the client side of every router call that started before the
// pause has returned: first up to timeout for the short calls to end by themselves, then, after it
// cancels what is left, up to timeout again for those to return. It does not wait for the guest:
// a request that left before its cancel can still reach the router after drain, which is why
// suspend verifies the state. It fails only when ctx ends.
func (g *gate) drain(ctx context.Context, timeout time.Duration) error {
	wait := func() (bool, error) {
		idle := g.idleChan()
		if idle == nil {
			return true, nil
		}
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-idle:
			return true, nil
		case <-t.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if done, err := wait(); done || err != nil {
		return err
	}
	g.cancelAll()
	_, err := wait()
	return err
}

// enter starts a short router call that must not wait for a suspended actor (Exec, Upload and their
// helpers): it fails when the gate is closed or the actor does not run. The caller calls end.
func (d *Driver) enter(ctx context.Context, op string, k key) (context.Context, func(), error) {
	return d.enterKind(ctx, op, k, shortCall)
}

func (d *Driver) enterKind(ctx context.Context, op string, k key, kind callKind) (context.Context, func(), error) {
	var cctx context.Context
	var end func()
	ok := false
	for {
		g := d.gate(k)
		if cctx, end, ok = g.begin(ctx, kind); ok || !g.isDead() {
			break
		}
	}
	if !ok {
		return nil, nil, fail(op, execution.ErrFailedPrecondition, "sandbox %s is being suspended or deleted", k)
	}
	if err := d.requireRunning(cctx, op, k); err != nil {
		end()
		d.forget(k)
		return nil, nil, err
	}
	return cctx, func() { end(); d.forget(k) }, nil
}

// enterWait is enter for a caller that outlives a suspend (the output stream, stdin): while the
// gate is closed or the actor does not run, it waits, then tries again.
//
// The proof that the actor runs must come after the registration, as in enter. A RUNNING read
// before the registration can be stale: a whole suspend (pause, SuspendActor, unpause) can run
// between the read and begin, and the open gate then admits a router request for a suspended
// actor, which resumes it. After the registration, a suspend that starts drains or cancels the
// read and the call.
//
// When the two reads disagree round after round (the state flaps between them), the rounds back
// off as waitRunning does: the first retry is at once, the next after PollInterval, doubling up to
// eight times PollInterval.
func (d *Driver) enterWait(ctx context.Context, op string, k key, kind callKind) (context.Context, func(), error) {
	var delay time.Duration
	for {
		cctx, end, err := d.enterKind(ctx, op, k, kind)
		if err == nil {
			return cctx, end, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, nil, cerr
		}
		if err := d.waitRunning(ctx, op, k); err != nil {
			return nil, nil, err
		}
		if delay > 0 {
			if err := sleep(ctx, delay); err != nil {
				return nil, nil, err
			}
		}
		delay = min(max(2*delay, d.cfg.PollInterval), 8*d.cfg.PollInterval)
	}
}

func (g *gate) unpause() {
	g.mu.Lock()
	g.paused--
	g.broadcast()
	g.mu.Unlock()
}

func (g *gate) broadcast() {
	close(g.wake)
	g.wake = make(chan struct{})
}

func (g *gate) state() (paused bool, wake <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused > 0, g.wake
}

func (g *gate) isDead() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dead
}

// addProc registers a running process on the gate of k.
func (d *Driver) addProc(k key, p *proc) {
	for {
		g := d.gate(k)
		g.mu.Lock()
		if !g.dead {
			g.procs[p] = struct{}{}
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
	}
}

// removeProc unregisters an ended process and drops the gate when it is idle. The process
// itself keeps the gate alive, so the gate is still in the map.
func (d *Driver) removeProc(k key, p *proc) {
	d.mu.Lock()
	g := d.gates[k]
	d.mu.Unlock()
	if g != nil {
		g.mu.Lock()
		delete(g.procs, p)
		g.mu.Unlock()
	}
	d.forget(k)
}

// waitRunning blocks until the actor runs and its gate is open, or ctx ends. It reads the control
// API, never the router, so waiting does not resume the actor. An actor that is crashed, being
// deleted or gone ends the wait with an error: it will not run again by itself.
func (d *Driver) waitRunning(ctx context.Context, op string, k key) error {
	defer d.forget(k) // a wait for an actor that is gone must not leave a gate behind
	delay := d.cfg.PollInterval
	for {
		g := d.gate(k) // fetched each round: an idle gate may have been dropped
		paused, wake := g.state()
		if !paused {
			actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
			switch {
			case err == nil:
				switch actor.GetStatus().GetState() {
				case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
					if p, _ := d.gate(k).state(); !p { // the gate may have closed while we read
						return nil
					}
					continue
				case ateapiv1.ActorState_ACTOR_STATE_CRASHED, ateapiv1.ActorState_ACTOR_STATE_DELETING:
					return fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s", k, phaseOf(actor))
				}
			case status.Code(err) == codes.Unavailable:
				// the control plane restarts; keep waiting
			default:
				return execution.GRPCError(op, err)
			}
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-wake:
			t.Stop()
		case <-t.C:
			delay = min(delay*2, 8*d.cfg.PollInterval)
		}
	}
}
