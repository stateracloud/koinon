// SPDX-License-Identifier: Apache-2.0

package substrate

// GateCount is the number of suspend gates the driver holds, for the tests that check they do not leak.
func (d *Driver) GateCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.gates)
}
