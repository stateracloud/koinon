// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
)

// GateCount is the number of suspend gates the driver holds, for the tests that check they do not leak.
func (d *Driver) GateCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.gates)
}

// PauseGate closes the gate of a sandbox as a suspend does first, for the specs of what a closed
// gate admits. The caller calls the returned unpause.
func (d *Driver) PauseGate(workspace, name string) (unpause func()) {
	return d.pauseGate(key{workspace, name}).unpause
}

// RouterCallsStarted is how many router calls the gate of a sandbox admitted. The count lives on the
// gate, so it is only stable while the gate is paused (an idle open gate may be dropped).
func (d *Driver) RouterCallsStarted(workspace, name string) uint64 {
	g := d.gate(key{workspace, name})
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.next
}

// SetExitGrace sets how long stdin.Close waits for the exit of a command whose close the guest failed.
func (d *Driver) SetExitGrace(grace time.Duration) { d.exitGrace = grace }

// Resuspends is the value of koinon_substrate_resuspends_total for one outcome.
func (d *Driver) Resuspends(outcome string) float64 {
	return testutil.ToFloat64(d.resuspends.WithLabelValues(outcome))
}

var (
	StoreError  = storeError
	ErrNoObject = errNoObject
)

// RenderTemplateForTest renders the ActorTemplate of a spec.
var RenderTemplateForTest = renderTemplate

// RouterCredentials builds the TLS credentials of the router connection.
var RouterCredentials = routerCredentials

// CheckActorTokenEffects is checkActorTokenEffects, for the spec that the actor JWT goes only to the egress rule.
var CheckActorTokenEffects = checkActorTokenEffects

// RulesEqual is rulesEqual, for the spec that the same routes give the same rules.
var RulesEqual = rulesEqual

// DialRouter is dialRouter, for the specs of a router that refuses the identity of the driver.
var DialRouter = dialRouter

// NewWithRouterRefusal is New with the refusal reader of a dialRouter connection.
func NewWithRouterRefusal(control, router grpc.ClientConnInterface, refusal func(error) error, cfg Config) *Driver {
	d := New(control, router, cfg)
	d.routerRefusal = refusal
	return d
}

// FrontUnreachable is frontUnreachable, for the specs of the router's two reports of a guest it could not
// reach.
var FrontUnreachable = frontUnreachable

// RepointInvariants is repointInvariants, for the specs of the template invariants of issue #1482.
var RepointInvariants = repointInvariants
