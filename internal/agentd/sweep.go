// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"
)

// sweepScript ends every process the sandbox user runs, apart from the sandbox's main process
// and the exec running the script (sweep.sh).
//
//go:embed sweep.sh
var sweepScript string

// sweepTimeout bounds a sweep: a TERM round, up to four KILL rounds, 3 s of waiting each.
const sweepTimeout = 30 * time.Second

// sweep ends what is left running in the sandbox under the agent's user. agentd sweeps before
// it starts a harness and after the harness has ended, so a run starts with no process of an
// earlier one: not a harness's daemon (Hermes's gateway idles up to 600 s and serves the next
// `hermes acp`), not a background job the agent started. Such a process could change the
// agent's home between the entry script's checks and the harness reading it, which would let
// the agent change its own tools or instructions for a later run (#84).
//
// It returns the IDs of the processes it ended, or a failure when some are still running.
func (r *runner) sweep(ctx context.Context, workspace, sandbox string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	out, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox, []string{"/bin/sh", "-c", sweepScript, "sweep"}, nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &failure{reason: fmt.Sprintf("end the processes left in the sandbox: exit %d: %s", code, strings.TrimSpace(stderr))}
	}
	ended, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "ended:")
	if !ok {
		return nil, &failure{reason: fmt.Sprintf("end the processes left in the sandbox: unexpected output %q", out)}
	}
	if ended = strings.TrimSpace(ended); ended == "none" {
		return nil, nil
	}
	return strings.Fields(ended), nil
}

// sweepAfter sweeps once the run's harness has ended. It is skipped when another worker may
// own the run by now, whose harness it would end; the next run's sweep catches what is left.
func (r *runner) sweepAfter(ctx context.Context, workspace, sandbox string) {
	if r.isFenced() && fence(r.fenceKind.Load()) != fenceCanceled {
		return
	}
	ended, err := r.sweep(context.WithoutCancel(ctx), workspace, sandbox)
	switch {
	case err != nil:
		r.log.Warn("ending the processes left in the sandbox failed", "err", err)
	case len(ended) > 0:
		r.log.Info("ended processes left in the sandbox", "pids", ended)
	}
}
