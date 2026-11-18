// SPDX-License-Identifier: Apache-2.0

package substrate

import "github.com/prometheus/client_golang/prometheus/testutil"

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

// Resuspends is the value of koinon_substrate_resuspends_total for one outcome.
func (d *Driver) Resuspends(outcome string) float64 {
	return testutil.ToFloat64(d.resuspends.WithLabelValues(outcome))
}
