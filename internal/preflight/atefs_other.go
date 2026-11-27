// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package preflight

import "errors"

// SharedFilesystems is what the probe pod runs; it needs Linux.
func SharedFilesystems() ([]string, bool, error) {
	return nil, false, errors.New("the /var/lib/ate probe runs on Linux only")
}
