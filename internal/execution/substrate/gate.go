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
// pauses the gate: that cancels the streams the driver holds to the guest and holds back every
// reconnect until the gate opens and the control API says the actor runs.
type gate struct {
	mu     sync.Mutex
	paused int
	wake   chan struct{} // closed and replaced whenever paused changes
	procs  map[*proc]struct{}
	next   uint64
	active map[uint64]context.CancelFunc // every router call in flight, cancelled by pause
	dead   bool                          // dropped from the driver's map: users fetch the gate again
}

func newGate() *gate {
	return &gate{wake: make(chan struct{}), procs: map[*proc]struct{}{}, active: map[uint64]context.CancelFunc{}}
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

// pause closes the gate and cancels every router call in flight. Calls nest. It reports false
// when the gate was dropped meanwhile.
func (g *gate) pause() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return false
	}
	g.paused++
	for id, cancel := range g.active {
		cancel()
		delete(g.active, id)
	}
	g.broadcast()
	return true
}

// begin registers one router call. The check of the gate and the registration happen under one
// lock, so a pause either sees the call and cancels it, or the call sees the pause and does not
// start: no call can slip between the two. The returned end must be called once the call is over.
func (g *gate) begin(parent context.Context) (ctx context.Context, end func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused > 0 || g.dead {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	g.next++
	id := g.next
	g.active[id] = cancel
	return ctx, func() {
		g.mu.Lock()
		delete(g.active, id)
		g.mu.Unlock()
		cancel()
	}, true
}

// enter starts a router call that must not wait for a suspended actor (Exec, Upload and their
// helpers): it fails when the gate is closed or the actor does not run. The caller calls end.
func (d *Driver) enter(ctx context.Context, op string, k key) (context.Context, func(), error) {
	var cctx context.Context
	var end func()
	ok := false
	for {
		g := d.gate(k)
		if cctx, end, ok = g.begin(ctx); ok || !g.isDead() {
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

// enterWait is enter for a caller that outlives a suspend (the output stream, stdin): it waits
// until the actor runs and the gate is open, then registers.
func (d *Driver) enterWait(ctx context.Context, op string, k key) (context.Context, func(), error) {
	for {
		if err := d.waitRunning(ctx, op, k); err != nil {
			return nil, nil, err
		}
		if cctx, end, ok := d.gate(k).begin(ctx); ok {
			return cctx, func() { end(); d.forget(k) }, nil
		}
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
