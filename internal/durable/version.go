// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// WorkflowVersion is the DBOS ApplicationVersion of every Koinon workflow (ADR 0085, decision 11).
// DBOS recovers a pending workflow only in a process with the same version, so the release version
// cannot serve: every upgrade would orphan what is pending. This value changes only when a
// registered workflow's steps change incompatibly, that is, when a workflow recorded by the previous
// code could not continue from the middle under the new code. TestWorkflowSteps fails when a
// workflow's step list changes without a bump; the bump is what makes CarryOver fork the older
// pending workflows from step 0.
//
// Rule for every workflow, old and new: it must be idempotent from step 0, because a carried-over
// workflow runs all its steps again. Give each effect a dedupe key (Once, a fixed workflow ID, an
// idempotent insert); see the table in ADR 0085.
const WorkflowVersion = "1"

// DefaultCarryOverInterval is how often a launched Runtime looks for pending workflows of another
// version. The first look is at Launch; later ones catch workflows that an older process, still
// running during a rolling upgrade, enqueues after that.
const DefaultCarryOverInterval = time.Minute

var (
	stepsMu  sync.Mutex
	stepList = map[string][]string{}
)

// DeclareSteps records the steps of a registered workflow, in order, and returns steps. Call it
// once per workflow, next to the code that runs the steps, and use the same constants for the
// step names (dbos.WithStepName), so the record and the code cannot drift apart unnoticed. The
// records feed TestWorkflowSteps, which compares them with testdata/workflow_steps.json.
func DeclareSteps(workflow string, steps ...string) []string {
	stepsMu.Lock()
	defer stepsMu.Unlock()
	if _, dup := stepList[workflow]; dup {
		panic("durable: steps of workflow " + workflow + " declared twice")
	}
	stepList[workflow] = append([]string(nil), steps...)
	return steps
}

// DeclaredSteps returns a copy of the declared step lists, by workflow name.
func DeclaredSteps() map[string][]string {
	stepsMu.Lock()
	defer stepsMu.Unlock()
	out := make(map[string][]string, len(stepList))
	for k, v := range stepList {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// CarryOver forks every pending, enqueued or delayed workflow whose application version is older than this
// runtime's (see olderVersion) from step 0 under this runtime's version, on the queue it was on, and cancels the
// original. The fork's ID is derived from the original's, so two processes that sweep at once, or a
// sweep that stopped between the fork and the cancel, produce one fork. It returns how many
// workflows it carried over; the error joins the ones it could not.
//
// The workflows must be idempotent from step 0 (see WorkflowVersion).
func (r *Runtime) CarryOver(ctx context.Context) (int, error) {
	dctx := r.Context()
	if dctx == nil {
		return 0, errors.New("durable: CarryOver before Launch")
	}
	version := r.version()
	old, err := listOtherVersions(dctx, version, r.cfg.Logger)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, wf := range old {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		forkID := carryOverID(wf.ID, version)
		_, err := dbos.ForkWorkflows[any](dctx, dbos.ForkWorkflowsInput{
			Workflows:          []dbos.ForkWorkflowSpec{{OriginalWorkflowID: wf.ID, ForkedWorkflowID: forkID, StartStep: 0}},
			ApplicationVersion: version,
			QueueName:          wf.QueueName,
			QueuePartitionKey:  wf.QueuePartitionKey,
		})
		if err != nil {
			// A fork that exists already (a sweep that stopped before the cancel) is as good as ours.
			found, lerr := dbos.ListWorkflows(dctx, dbos.WithFilterWorkflowIDs(forkID), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
			if lerr != nil || len(found) == 0 {
				errs = append(errs, fmt.Errorf("fork workflow %s (%s, version %q): %w", wf.ID, wf.Name, wf.ApplicationVersion, err))
				continue
			}
		}
		if err := dbos.CancelWorkflow(dctx, wf.ID); err != nil {
			errs = append(errs, fmt.Errorf("cancel carried-over workflow %s: %w", wf.ID, err))
			continue
		}
		r.cfg.Logger.Info("durable: carried a pending workflow over to the current version, from step 0",
			"workflow", wf.Name, "id", wf.ID, "fork", forkID, "from_version", wf.ApplicationVersion, "version", version)
		n++
	}
	return n, errors.Join(errs...)
}

func carryOverID(original, version string) string {
	return original + "~v" + version
}

// olderVersion reports whether a workflow of version wf is to be carried over by a runtime of
// version cur. WorkflowVersion is an integer string, so a workflow is carried over only when its
// version is lower. A version that is not an integer (the legacy release versions such as
// 0.1.0-rc.N, which no current process runs) counts as older. A newer version belongs to a
// process of a rolling upgrade or to a rollback target and is never touched: forking it back would
// run new step layouts on old code and ping-pong between the two versions.
func olderVersion(wf, cur string) bool {
	w, err := strconv.ParseUint(wf, 10, 64)
	if err != nil {
		return true
	}
	c, err := strconv.ParseUint(cur, 10, 64)
	if err != nil {
		return false
	}
	return w < c
}

// listOtherVersions lists the workflows of an older version that wait for a process of this one, oldest first.
// DBOS cannot filter on "not this version", so it lists by status and filters here; the list is
// complete before any is changed, so paging is not disturbed by the cancels.
func listOtherVersions(dctx dbos.Context, version string, logger *slog.Logger) ([]dbos.WorkflowStatus, error) {
	const page = 500
	var out []dbos.WorkflowStatus
	for offset := 0; ; offset += page {
		got, err := dbos.ListWorkflows(dctx,
			dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false),
			dbos.WithFilterLimit(page), dbos.WithFilterOffset(offset))
		if err != nil {
			return nil, fmt.Errorf("durable: list pending workflows: %w", err)
		}
		for _, wf := range got {
			if wf.ApplicationVersion == version {
				continue
			}
			if !olderVersion(wf.ApplicationVersion, version) {
				logger.Info("durable: leaving a pending workflow of a newer or incomparable version alone",
					"workflow", wf.Name, "id", wf.ID, "version", wf.ApplicationVersion, "this_version", version)
				continue
			}
			out = append(out, wf)
		}
		if len(got) < page {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// version is the ApplicationVersion this runtime launches with.
func (r *Runtime) version() string {
	if r.cfg.ApplicationVersion != "" {
		return r.cfg.ApplicationVersion
	}
	return WorkflowVersion
}

// carryOverLoop sweeps every interval until ctx ends.
func (r *Runtime) carryOverLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

func (r *Runtime) sweep(ctx context.Context) {
	n, err := r.CarryOver(ctx)
	if err != nil && ctx.Err() == nil {
		r.cfg.Logger.Error("durable: carrying pending workflows of another version over failed, retrying at the next sweep", "carried", n, "err", err)
	}
}
