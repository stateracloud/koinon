// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ownedRecheck is how long the reaper waits before it looks again at a zombie that the tracker owns.
// It only guards against a late state update in the tracker. The tracker's own wait takes the zombie
// first in nearly every case, so the reaper is idle.
const ownedRecheck = time.Second

// zombie names one zombie child of the front.
type zombie struct {
	PID int
}

// reaper reaps the orphans that the front adopts as PID 1.
//
// The front is PID 1 in the guest. When a process dies, its children go to PID 1, and PID 1 must
// wait for them or they stay as zombies. Each zombie counts against pids.max of the sandbox.
//
// A plain wait4(-1) loop is wrong here. The upstream guest server starts the payload with os/exec
// and waits for each child by PID (cmd.Wait). A wait4(-1) would take the exit status of those
// children, and cmd.Wait would fail with ECHILD. A timer is wrong too: under load cmd.Wait can be
// late by more than any fixed time. So the front records the PID of each process that it starts
// (ownedProcesses) and the reaper takes only a zombie whose PID is not recorded, by exact PID.
// This is the rule of tini and dumb-init (take every child) narrowed by the PIDs that the front
// itself waits for.
type reaper struct {
	logger *slog.Logger
	// list returns the zombie children of this process.
	list func() ([]zombie, error)
	// reap waits for one zombie by PID. It reports whether it took the status.
	reap func(pid int) bool
	// owns reports whether the front started the process and still waits for it.
	owns func(pid int) bool
	// exclusive stops the start of new processes until the returned function runs.
	exclusive func() func()
}

func newReaper(own *ownedProcesses, logger *slog.Logger) *reaper {
	return &reaper{
		logger: logger,
		list:   listZombieChildren, reap: reapPID,
		owns: own.owns, exclusive: own.exclusive,
	}
}

// run reaps until ctx ends. It wakes on SIGCHLD. It does not poll when nothing is waiting.
func (r *reaper) run(ctx context.Context) {
	enableSubreaper(r.logger)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	defer signal.Stop(sig)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	// Scan once at the start: orphans can exist before the first signal.
	again := r.scan()
	for {
		if again {
			timer.Reset(ownedRecheck)
		}
		select {
		case <-ctx.Done():
			return
		case <-sig:
		case <-timer.C:
		}
		timer.Stop()
		again = r.scan()
	}
}

// scan reaps the zombie children that the front did not start. It reports whether it left a zombie
// that the tracker owns, so the caller looks again later.
func (r *reaper) scan() (again bool) {
	// No process starts during the scan, so every process that is running is in the set.
	defer r.exclusive()()
	zs, err := r.list()
	if err != nil {
		r.logger.Warn("guest front: list zombie children", "err", err)
		return true
	}
	for _, z := range zs {
		if r.owns(z.PID) {
			again = true
			continue
		}
		if r.reap(z.PID) {
			r.logger.Info("guest front: reaped an orphan", "pid", z.PID)
		}
	}
	return again
}

// reapPID waits for one child by PID and does not block.
func reapPID(pid int) bool {
	var ws syscall.WaitStatus
	for {
		p, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		return err == nil && p == pid
	}
}
