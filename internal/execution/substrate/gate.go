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
}

func newGate() *gate { return &gate{wake: make(chan struct{}), procs: map[*proc]struct{}{}} }

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

// forget drops the gate of an actor that is gone, once nothing uses it.
func (d *Driver) forget(k key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if g, ok := d.gates[k]; ok {
		g.mu.Lock()
		idle := len(g.procs) == 0 && g.paused == 0
		g.mu.Unlock()
		if idle {
			delete(d.gates, k)
		}
	}
}

// pause closes the gate and drops the live streams. Calls nest.
func (g *gate) pause() {
	g.mu.Lock()
	g.paused++
	procs := make([]*proc, 0, len(g.procs))
	for p := range g.procs {
		procs = append(procs, p)
	}
	g.broadcast()
	g.mu.Unlock()
	for _, p := range procs {
		p.dropStream()
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

func (g *gate) add(p *proc) {
	g.mu.Lock()
	g.procs[p] = struct{}{}
	g.mu.Unlock()
}

func (g *gate) remove(p *proc) {
	g.mu.Lock()
	delete(g.procs, p)
	g.mu.Unlock()
}

// waitRunning blocks until the actor runs and its gate is open, or ctx ends. It reads the control
// API, never the router, so waiting does not resume the actor. An actor that is crashed, being
// deleted or gone ends the wait with an error: it will not run again by itself.
func (d *Driver) waitRunning(ctx context.Context, op string, k key) error {
	g := d.gate(k)
	delay := d.cfg.PollInterval
	for {
		paused, wake := g.state()
		if !paused {
			actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(k.atespace, k.actor)})
			switch {
			case err == nil:
				switch actor.GetStatus().GetState() {
				case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
					if p, _ := g.state(); !p { // the gate may have closed while we read
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
