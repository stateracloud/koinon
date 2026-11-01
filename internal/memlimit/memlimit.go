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
	"strings"
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

// Where the container's own limit is written: cgroup v2, then v1; and the host's memory, which a
// "limit" at or above is no limit. Variables so the tests can point them at files.
var (
	cgroupFiles = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}
	meminfoPath = "/proc/meminfo"
)

// Limit returns the container's memory limit in bytes, or 0 when there is none. The cgroup is the
// truth when it can be read: a container without a memory limit gets the node's allocatable memory
// from the downward API in Env, which is no limit at all, but its cgroup says "max" (or the host's
// memory), and then there is none. With a cgroup limit the smaller of it and Env counts. When the
// cgroup cannot be read (another OS, no mount), Env alone is used. Components that hold memory on
// behalf of callers (the trace router's queues) size themselves from it.
func Limit() int64 {
	env := envLimit()
	cg, known := cgroupLimit()
	switch {
	case !known:
		return env
	case cg == 0:
		return 0
	case env > 0:
		return min(env, cg)
	}
	return cg
}

func envLimit() int64 {
	n, err := strconv.ParseInt(os.Getenv(Env), 10, 64)
	if err != nil || n <= 0 || n > 1<<50 {
		return 0
	}
	return n
}

// cgroupLimit reads the cgroup's memory limit. known is false when no file could be read or parsed;
// "max" and a value at or above the host's memory (cgroup v1 writes a huge number for none) are known
// to be no limit and give 0.
func cgroupLimit() (limit int64, known bool) {
	host := hostMemory()
	for _, f := range cgroupFiles {
		raw, err := os.ReadFile(f) //nolint:gosec // a fixed cgroup path (tests substitute another)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(raw))
		if v == "max" {
			return 0, true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		switch {
		case err != nil || n <= 0:
			return 0, false
		case n > 1<<50 || host > 0 && n >= host:
			return 0, true
		}
		return n, true
	}
	return 0, false
}

// hostMemory is MemTotal from /proc/meminfo in bytes, or 0 when unknown.
func hostMemory() int64 {
	raw, err := os.ReadFile(meminfoPath)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil && kb > 0 {
					return kb * 1024
				}
			}
		}
	}
	return 0
}
