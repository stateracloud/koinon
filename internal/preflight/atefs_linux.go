// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"fmt"
	"syscall"
)

// SameFilesystem is what the probe pod runs: it reports whether the node root and AteDir, as mounted in the
// pod, are on one filesystem (the same device number).
func SameFilesystem() (bool, error) {
	var root, ate syscall.Stat_t
	if err := syscall.Stat(ateFSProbeRoot, &root); err != nil {
		return false, fmt.Errorf("stat %s: %w", ateFSProbeRoot, err)
	}
	if err := syscall.Stat(ateFSProbeAte, &ate); err != nil {
		return false, fmt.Errorf("stat %s: %w", ateFSProbeAte, err)
	}
	return uint64(root.Dev) == uint64(ate.Dev), nil //nolint:unconvert // Dev is int32 on some platforms
}
