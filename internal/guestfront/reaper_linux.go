// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// enableSubreaper makes this process the parent of the orphans in its tree. As PID 1 it is that
// already. The call makes the reaper work in a process that is not PID 1 (a test, a debug run).
func enableSubreaper(logger *slog.Logger) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		logger.Warn("guest front: set child subreaper", "err", err)
	}
}

// listZombieChildren reads /proc for the zombie children of this process.
func listZombieChildren() ([]zombie, error) {
	return zombieChildrenIn("/proc", os.Getpid())
}

func zombieChildrenIn(proc string, parent int) ([]zombie, error) {
	ents, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	var out []zombie
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "stat")) //nolint:gosec // G304: a numeric entry of /proc
		if err != nil {
			continue // the process ended after the listing
		}
		if z, ok := parseZombie(pid, string(b), parent); ok {
			out = append(out, z)
		}
	}
	return out, nil
}

// parseZombie reads a /proc/<pid>/stat line. The name in field 2 can hold spaces and brackets, so
// the fields start after the last ")".
func parseZombie(pid int, stat string, parent int) (zombie, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return zombie{}, false
	}
	f := strings.Fields(stat[i+1:])
	// f[0] state, f[1] ppid.
	if len(f) < 2 || f[0] != "Z" {
		return zombie{}, false
	}
	if ppid, err := strconv.Atoi(f[1]); err != nil || ppid != parent {
		return zombie{}, false
	}
	return zombie{PID: pid}, true
}
