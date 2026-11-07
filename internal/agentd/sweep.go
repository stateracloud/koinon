// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sweepScript ends every process the sandbox user runs, apart from the sandbox's main process
// and the exec running the script (sweep.sh).
//
//go:embed sweep.sh
var sweepScript string

// procinfoScript reads the processes the sweep could not end (procinfo.sh).
//
//go:embed procinfo.sh
var procinfoScript string

// saveworkScript reads the workspace's uncommitted work (savework.sh).
//
//go:embed savework.sh
var saveworkScript string

// sweepTimeout bounds a sweep: a TERM round, up to four KILL rounds, 3 s of waiting each.
const sweepTimeout = 30 * time.Second

// sweep ends what is left running in the sandbox under the agent's user. agentd sweeps before
// it starts a harness and after the harness has ended, so a run starts with no process of an
// earlier one: not a harness's daemon (one that idles after a run and serves the next
// session), not a background job the agent started. Such a process could change the
// agent's home between the entry script's checks and the harness reading it, which would let
// the agent change its own tools or instructions for a later run (old issue 84).
//
// It returns the main process it kept and the IDs of the processes it ended, or a failure
// when some are still running.
func (r *runner) sweep(ctx context.Context, workspace, sandbox string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	out, code, stderr, err := r.sandboxExec(ctx, workspace, sandbox, []string{"/bin/sh", "-c", sweepScript, "sweep"}, nil)
	if err != nil {
		return "", nil, err
	}
	if code == 1 {
		// Processes survived the last SIGKILL round. They may be stuck for good (stuck.go).
		if sv := parseSurvivors(string(out)); len(sv) > 0 {
			return "", nil, &survivorsError{survivors: sv, detail: strings.TrimSpace(stderr)}
		}
	}
	if code != 0 {
		return "", nil, &failure{reason: fmt.Sprintf("end the processes left in the sandbox: exit %d: %s", code, strings.TrimSpace(stderr))}
	}
	var main, ended string
	found := false
	for line := range strings.Lines(string(out)) {
		if v, ok := strings.CutPrefix(line, "main:"); ok {
			main = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "ended:"); ok {
			ended, found = strings.TrimSpace(v), true
		}
	}
	if !found {
		return "", nil, &failure{reason: fmt.Sprintf("end the processes left in the sandbox: unexpected output %q", out)}
	}
	if ended == "none" {
		return main, nil, nil
	}
	return main, strings.Fields(ended), nil
}

// sweepAfter sweeps once the run's harness has ended, while the run still holds the agent's
// active slot. It is skipped once the worker is fenced: the run was taken over, canceled or
// given up, its slot may be free, and the agent's next run may have started its harness. The
// next run's sweep catches what is left.
func (r *runner) sweepAfter(ctx context.Context, workspace, sandbox string) {
	if r.own.IsFenced() {
		return
	}
	_, ended, err := r.sweep(context.WithoutCancel(ctx), workspace, sandbox)
	var sv *survivorsError
	switch {
	case errors.As(err, &sv):
		r.log.Warn("processes survived the end-of-turn sweep", "survivors", sv.survivors)
		r.stuckAfterTurn(ctx, workspace, sandbox, sv)
	case err != nil:
		r.log.Warn("ending the processes left in the sandbox failed", "err", err)
	case len(ended) > 0:
		r.log.Info("ended processes left in the sandbox", "pids", ended)
	}
}
