// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package preflight

import "errors"

// SameFilesystem is what the probe pod runs; it needs Linux.
func SameFilesystem() (bool, error) {
	return false, errors.New("the /var/lib/ate probe runs on Linux only")
}
