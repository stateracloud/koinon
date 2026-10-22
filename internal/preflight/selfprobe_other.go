// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package preflight

import "errors"

// SelfProbe is what the probe pod runs; it needs Linux.
func SelfProbe() (ProbeResult, error) {
	return ProbeResult{}, errors.New("the seccomp probe runs on Linux only")
}
