// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// waitSandbox waits until the agent's sandbox is ready and returns its row: the workspace, the
// name and the home generation the harness session must belong to.
//
// koinon-reconciler is the one source of sandbox state: it renders the sandbox, records what
// it observes in agent_sandboxes and is the only component that starts or stops it. agentd
// reads that row. A stopped sandbox (idle stop, ADR 0049) is started by the reconciler because
// this run, now starting, is demand for it (GetAgentRunActivity); while the sandbox is not
// ready agentd NOTIFYs the reconciler every ReconcileNudge, so a pass follows within a second
// rather than a resync interval (a takeover, for example, changes no run state and so fires no
// trigger). A missing row is waited for, since the reconciler creates it when the agent is
// activated. The run fails if the sandbox is not ready within SandboxTimeout.
func (r *runner) waitSandbox(ctx context.Context) (store.AgentSandbox, error) {
	started := time.Now()
	defer func() { r.p.cfg.Metrics.SandboxWait.Observe(time.Since(started).Seconds()) }()
	deadline := started.Add(r.p.cfg.SandboxTimeout)
	var nudged time.Time
	var last string
	for {
		sb, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: r.run.OrgID, AgentID: r.run.AgentID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			last = "not rendered yet"
		case err != nil:
			if ctx.Err() != nil {
				return store.AgentSandbox{}, ctx.Err()
			}
			last = err.Error()
		case sb.State == store.SandboxStateReady && sb.UnhealthyAt.Valid:
			// Found stuck: the reconciler recreates it (ADR 0054).
			last = "recreating the stuck sandbox"
		case sb.State == store.SandboxStateReady:
			return sb, nil
		case sb.State == store.SandboxStateError && r.agentHealthy(ctx):
			// The reconciler recreates a failed sandbox once no run is active (and gives up by
			// marking the agent unhealthy), so wait for it, within SandboxTimeout.
			last = "error"
			if sb.Message != "" {
				last += ": " + sb.Message
			}
		case sb.State == store.SandboxStateError, sb.State == store.SandboxStateDeleted,
			sb.State == store.SandboxStateRetained:
			reason := "sandbox " + string(sb.State)
			if sb.Phase != "" {
				reason += " (" + sb.Phase + ")"
			}
			if sb.Message != "" {
				reason += ": " + sb.Message
			}
			return store.AgentSandbox{}, &failure{reason: reason}
		default: // pending, starting, stopping, stopped, deleting
			// Deleting is a recreation in progress: for a changed spec, a stuck sandbox (ADR 0054)
			// or one found in Error or Completed. The next pass creates the new sandbox, so the
			// run waits for it, within SandboxTimeout, instead of failing.
			last = string(sb.State)
		}
		if time.Since(nudged) >= r.p.cfg.ReconcileNudge {
			if nudged.IsZero() {
				r.log.Info("waiting for the reconciler to make the sandbox ready", "state", last)
			}
			nudged = time.Now()
			if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil && ctx.Err() == nil {
				r.log.Warn("notifying the reconciler failed", "err", err)
			}
		}
		if time.Now().After(deadline) {
			return store.AgentSandbox{}, &failure{reason: fmt.Sprintf("sandbox not ready after %s (%s)", r.p.cfg.SandboxTimeout, last)}
		}
		if !sleep(ctx, r.p.cfg.Poll) {
			return store.AgentSandbox{}, ctx.Err()
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
	return r.sandboxExecLimit(ctx, workspace, name, cmd, stdin, time.Minute, 1<<20)
}

// sandboxExecLimit is sandboxExec with its own timeout and cap on the output it reads.
func (r *runner) sandboxExecLimit(ctx context.Context, workspace, name string, cmd []string, stdin []byte, timeout time.Duration, limit int64) ([]byte, int, string, error) {
	var stderr tail
	proc, err := r.p.backend.Exec(ctx, workspace, name, execution.ExecRequest{
		Command: cmd, Stderr: &stderr, Timeout: timeout,
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
	out, _ := io.ReadAll(io.LimitReader(proc.Stdout(), limit))
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

// agentHealthy reports whether the agent is not marked unhealthy (ADR 0054). When it cannot tell,
// it says no, so the run fails at once as it always did.
func (r *runner) agentHealthy(ctx context.Context) bool {
	a, err := r.q.GetAgent(ctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
	return err == nil && !a.UnhealthyAt.Valid
}
