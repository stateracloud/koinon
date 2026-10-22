// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/procoduck/koinon/internal/execution"
)

// waitSandbox waits until the agent's sandbox is ready. A stopped sandbox (idle stop, plan
// item R2) is started; a missing one is waited for, since the reconciler creates it when the
// agent is activated.
func (r *runner) waitSandbox(ctx context.Context, workspace, name string) error {
	deadline := time.Now().Add(r.p.cfg.SandboxTimeout)
	started := false
	var last string
	for {
		sb, err := r.p.backend.GetSandbox(ctx, workspace, name)
		switch {
		case errors.Is(err, execution.ErrNotFound):
			last = "not created yet"
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, execution.ErrPermissionDenied) || errors.Is(err, execution.ErrInvalid) {
				return &failure{reason: "sandbox: " + err.Error()}
			}
			last = err.Error()
		case sb.Phase == execution.PhaseReady:
			return nil
		case sb.Phase == execution.PhaseStopped:
			last = "stopped"
			if !started {
				r.log.Info("starting the agent's sandbox", "sandbox", name)
				_, err := r.p.backend.StartSandbox(ctx, workspace, name)
				// FailedPrecondition: someone else started it first.
				if err != nil && !errors.Is(err, execution.ErrFailedPrecondition) {
					last = "start: " + err.Error()
				} else {
					started = true
				}
			}
		case sb.Phase == execution.PhaseError, sb.Phase == execution.PhaseCompleted, sb.Phase == execution.PhaseDeleting:
			reason := string(sb.Phase)
			if sb.Reason != "" {
				reason += ": " + sb.Reason
			}
			if sb.Message != "" {
				reason += ": " + sb.Message
			}
			return &failure{reason: "sandbox " + reason}
		default:
			last = string(sb.Phase)
		}
		if time.Now().After(deadline) {
			return &failure{reason: fmt.Sprintf("sandbox not ready after %s (%s)", r.p.cfg.SandboxTimeout, last)}
		}
		if !sleep(ctx, r.p.cfg.Poll) {
			return ctx.Err()
		}
	}
}

// mkdir creates dir in the sandbox. Harnesses refuse a session whose directory is missing.
func (r *runner) mkdir(ctx context.Context, workspace, name, dir string) error {
	_, code, stderr, err := r.sandboxExec(ctx, workspace, name, []string{"/bin/mkdir", "-p", dir}, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return &failure{reason: fmt.Sprintf("mkdir %s exited with %d: %s", dir, code, stderr)}
	}
	return nil
}

// sandboxExec runs a short command in the sandbox with stdin as its input and returns its
// output, exit code and the tail of its standard error.
func (r *runner) sandboxExec(ctx context.Context, workspace, name string, cmd []string, stdin []byte) ([]byte, int, string, error) {
	var stderr tail
	proc, err := r.p.backend.Exec(ctx, workspace, name, execution.ExecRequest{
		Command: cmd, Stderr: &stderr, Timeout: time.Minute,
	})
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = proc.Close() }()
	werr := make(chan error, 1)
	go func() {
		_, err := proc.Stdin().Write(stdin)
		werr <- errors.Join(err, proc.Stdin().Close())
	}()
	out, _ := io.ReadAll(io.LimitReader(proc.Stdout(), 1<<20))
	code, err := proc.Wait()
	if err != nil {
		return nil, 0, "", err
	}
	if err := <-werr; err != nil && code == 0 {
		return nil, 0, "", err
	}
	return out, code, stderr.String(), nil
}

// closeProcess ends a harness: end-of-file on stdin first, so it can exit cleanly, then the
// stream.
func closeProcess(p execution.Process) {
	_ = p.Stdin().Close()
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	_ = p.Close()
}

// tail keeps the last bytes written to it, for error reasons. Harness stderr can be long.
type tail struct {
	mu sync.Mutex
	b  []byte
}

const tailSize = 2048

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = append([]byte(nil), t.b[len(t.b)-tailSize:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
