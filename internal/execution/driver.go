// SPDX-License-Identifier: Apache-2.0

package execution

import "fmt"

// DriverName names an execution driver. It is the value of the install-wide setting
// `execution.driver` (epic 1082, ADR 0119 D9): there is no per-org mix.
type DriverName string

// The drivers. Substrate is the default since #1093; OpenShell stays selectable until #1094 removes it.
const (
	DriverOpenShell DriverName = "openshell"
	DriverSubstrate DriverName = "substrate"

	// DefaultDriver is what an install that sets nothing runs.
	DefaultDriver = DriverSubstrate
)

// ParseDriver reads the value of `execution.driver`. The empty string is the default.
func ParseDriver(s string) (DriverName, error) {
	switch DriverName(s) {
	case "":
		return DefaultDriver, nil
	case DriverOpenShell, DriverSubstrate:
		return DriverName(s), nil
	}
	return "", fmt.Errorf("execution.driver %q is not one of %q, %q", s, DriverOpenShell, DriverSubstrate)
}

// HasEgress reports whether the driver implements the [Egress] half of the seam. Substrate does
// not: its egress is the gateway's (ADR 0097), so code that still needs [Backend] cannot run on it.
func (d DriverName) HasEgress() bool { return d == DriverOpenShell }
