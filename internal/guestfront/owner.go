// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"context"
	"sync"

	"github.com/agent-substrate/env/guest/process"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// ownedProcesses is the set of processes that the front started itself, for the reaper.
//
// The upstream process tracker starts each payload with os/exec and waits for it by PID. Such a wait
// must be the one that takes the exit status. The reaper must not take the zombie of a process that
// the tracker still waits for, however late that wait is. So the reaper reaps a zombie only when its
// PID is not in this set.
//
// The tracker is the upstream package, unchanged. The front wraps its service: StartProcess adds the
// PID of the new process. The state of the process in the tracker says when its wait is over, so
// there is no separate removal step to keep in step.
type ownedProcesses struct {
	svc *process.Service

	// gate makes the start of a process and its registration one step for the reaper. A start holds
	// it shared. A scan of the reaper holds it exclusive. Without it, a process could start, exit and
	// be reaped before its PID is in the set.
	gate sync.RWMutex

	mu    sync.Mutex
	byPID map[int]string // PID to process id
}

func newOwnedProcesses(svc *process.Service) *ownedProcesses {
	return &ownedProcesses{svc: svc, byPID: map[int]string{}}
}

// ownedService is the upstream ProcessService with a record of the processes that it starts.
type ownedService struct {
	*process.Service
	own *ownedProcesses
}

func (s *ownedService) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	s.own.gate.RLock()
	defer s.own.gate.RUnlock()
	p, err := s.Service.StartProcess(ctx, req)
	if err != nil {
		return nil, err
	}
	s.own.mu.Lock()
	s.own.byPID[int(p.GetPid())] = p.GetProcessId()
	s.own.mu.Unlock()
	return p, nil
}

// exclusive blocks the start of new processes until the returned function runs.
func (o *ownedProcesses) exclusive() func() {
	o.gate.Lock()
	return o.gate.Unlock
}

// owns reports whether the tracker started pid and its wait on pid is not over. After the wait the
// PID is free for the kernel to reuse, so a later zombie with this PID is an orphan.
func (o *ownedProcesses) owns(pid int) bool {
	o.mu.Lock()
	id, ok := o.byPID[pid]
	o.mu.Unlock()
	if !ok {
		return false
	}
	p, err := o.svc.GetProcess(context.Background(), &ateenvv1alpha.GetProcessRequest{ProcessId: id})
	if err == nil && p.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		return true
	}
	o.mu.Lock()
	if o.byPID[pid] == id {
		delete(o.byPID, pid) // the tracker forgot it, or its wait is over
	}
	o.mu.Unlock()
	return false
}
