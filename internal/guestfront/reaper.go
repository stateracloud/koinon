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

// DefaultReapGrace is how long a zombie child must stay unreaped before the reaper takes it.
const DefaultReapGrace = time.Second

// zombie names one zombie child of the front. Start is the process start time from the kernel,
// so a reused PID is a different zombie.
type zombie struct {
	PID   int
	Start uint64
}

// reaper reaps the orphans that the front adopts as PID 1.
//
// The front is PID 1 in the guest. When a process dies, its children go to PID 1, and PID 1 must
// wait for them or they stay as zombies. Each zombie counts against pids.max of the sandbox.
//
// A plain wait4(-1) loop is wrong here. The upstream guest server starts the payload with os/exec
// and waits for each child by PID (cmd.Wait). A wait4(-1) would take the exit status of those
// children, and cmd.Wait would fail with ECHILD. The reaper cannot tell a child that the front
// started from an adopted orphan, so it uses time: cmd.Wait starts right after the start of the
// child and reaps it at once when it exits. A zombie that is still there after a grace period is an
// orphan, and the reaper waits for exactly that PID.
type reaper struct {
	grace  time.Duration
	logger *slog.Logger
	// list returns the zombie children of this process.
	list func() ([]zombie, error)
	// reap waits for one zombie. It reports whether it took the status.
	reap func(pid int) bool

	seen map[zombie]time.Time
}

func newReaper(grace time.Duration, logger *slog.Logger) *reaper {
	if grace <= 0 {
		grace = DefaultReapGrace
	}
	return &reaper{
		grace: grace, logger: logger,
		list: listZombieChildren, reap: reapPID,
		seen: map[zombie]time.Time{},
	}
}

// run reaps until ctx ends. It wakes on SIGCHLD, and after a scan that leaves a young zombie it wakes
// again when the grace period of that zombie ends. It does not poll when nothing is waiting.
func (r *reaper) run(ctx context.Context) {
	enableSubreaper(r.logger)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	defer signal.Stop(sig)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	// Scan once at the start: orphans can exist before the first signal.
	next := r.scan(time.Now())
	for {
		if next > 0 {
			timer.Reset(next)
		}
		select {
		case <-ctx.Done():
			return
		case <-sig:
		case <-timer.C:
		}
		timer.Stop()
		next = r.scan(time.Now())
	}
}

// scan reaps the zombies that are older than the grace period. It returns the time until the next
// scan is due, or 0 when no zombie is waiting.
func (r *reaper) scan(now time.Time) time.Duration {
	zs, err := r.list()
	if err != nil {
		r.logger.Warn("guest front: list zombie children", "err", err)
		return r.grace
	}
	live := make(map[zombie]struct{}, len(zs))
	var next time.Duration
	for _, z := range zs {
		live[z] = struct{}{}
		first, ok := r.seen[z]
		if !ok {
			r.seen[z] = now
			first = now
		}
		if age := now.Sub(first); age < r.grace {
			if left := r.grace - age; next == 0 || left < next {
				next = left
			}
			continue
		}
		if r.reap(z.PID) {
			r.logger.Debug("guest front: reaped an orphan", "pid", z.PID)
		}
		delete(r.seen, z)
		delete(live, z)
	}
	for z := range r.seen {
		if _, ok := live[z]; !ok {
			delete(r.seen, z)
		}
	}
	return next
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
