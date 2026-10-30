// SPDX-License-Identifier: Apache-2.0

// Package memlimit makes the Go runtime respect the container's memory limit.
//
// The chart passes the container's memory limit through the downward API in bytes
// (KOINON_MEMORY_LIMIT_BYTES). GOMEMLIMIT cannot take a percentage, so each binary calls Apply at
// startup, which sets the runtime's soft limit to 80% of it. Without a soft limit the garbage
// collector lets the heap grow to the hard limit and the kernel kills the pod.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
)

// Env is the variable holding the container's memory limit in bytes.
const Env = "KOINON_MEMORY_LIMIT_BYTES"

// Percent of the limit that becomes the runtime's soft limit.
const Percent = 80

// Apply sets the soft memory limit from Env, unless GOMEMLIMIT is set (an operator's choice wins).
// It returns the limit it set, or 0 when it set none.
func Apply() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	n, err := strconv.ParseInt(os.Getenv(Env), 10, 64)
	if err != nil || n <= 0 || n > 1<<50 {
		return 0
	}
	limit := n / 100 * Percent
	debug.SetMemoryLimit(limit)
	return limit
}

// Limit returns the container's memory limit in bytes from Env, or 0 when it is not set. Components
// that hold memory on behalf of callers (the trace router's queues) size themselves from it.
func Limit() int64 {
	n, err := strconv.ParseInt(os.Getenv(Env), 10, 64)
	if err != nil || n <= 0 || n > 1<<50 {
		return 0
	}
	return n
}
