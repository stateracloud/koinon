// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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
// idempotent insert); see the table in ADR 0085. A new workflow is a bump too: a process of the
// previous version would dequeue it without having it registered.
//
// History: 2 added koinon.code-host-poll-event (issue #25).
const WorkflowVersion = "2"

// DefaultCarryOverInterval is how often a launched Runtime looks for pending workflows of another
// version. The first look is at Launch; later ones catch workflows that an older process, still
// running during a rolling upgrade, enqueues after that.
const DefaultCarryOverInterval = time.Minute

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

// CarriedOverID is the ID of the fork that CarryOver makes of workflow id under this runtime's
// version. A caller that enqueues under a fixed ID (so a retried request attaches to the same
// workflow) uses AwaitResult instead of deriving it.
func (r *Runtime) CarriedOverID(id string) string {
	return carryOverID(id, r.version())
}

// maxCarryOverHops bounds how far AwaitResult follows a chain of carry-overs (one per bump of
// WorkflowVersion that the workflow lived through).
const maxCarryOverHops = 8

// AwaitResult waits for the result of h, the handle of workflow id, for at most timeout in all
// (zero or less waits without limit). When the workflow was cancelled by CarryOver, whether before
// the call (a client retry enqueues under the original ID and DBOS hands back the cancelled
// workflow) or while it waited, the result is the fork's, so the caller sees what the carried-over
// run produced instead of a cancellation error. A workflow that lived through several bumps was
// forked more than once (id~v2~v3); AwaitResult follows the chain, at most maxCarryOverHops
// forks, while the newest one is cancelled too. One deadline covers the whole wait. Any other
// error, and a cancellation with no fork, is returned as it is.
func AwaitResult[R any](r *Runtime, h dbos.WorkflowHandle[R], id string, timeout time.Duration) (R, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	cur := id
	for hop := 0; ; hop++ {
		var opts []dbos.GetResultOption
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				var zero R
				return zero, fmt.Errorf("durable: waiting for workflow %s: %w", cur, dbos.ErrTimeout)
			}
			opts = append(opts, dbos.WithHandleTimeout(left))
		}
		out, err := h.GetResult(opts...)
		if err == nil || (!errors.Is(err, dbos.ErrWorkflowCancelled) && !errors.Is(err, dbos.ErrAwaitedWorkflowCancelled)) {
			return out, err
		}
		dctx := r.Context()
		if dctx == nil || hop >= maxCarryOverHops {
			return out, err
		}
		next, ok := newestFork(dctx, cur)
		if !ok {
			return out, err
		}
		fork, ferr := dbos.RetrieveWorkflow[R](dctx, next)
		if ferr != nil {
			return out, err
		}
		h, cur = fork, next
	}
}

// newestFork returns the ID of the most recently created carry-over fork below workflow id
// (id~v2, or id~v2~v3 when the fork was carried over again).
func newestFork(dctx dbos.Context, id string) (string, bool) {
	found, err := dbos.ListWorkflows(dctx, dbos.WithFilterWorkflowIDPrefix(id+"~v"),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil || len(found) == 0 {
		return "", false
	}
	newest := found[0]
	for _, wf := range found[1:] {
		if wf.CreatedAt.After(newest.CreatedAt) {
			newest = wf
		}
	}
	return newest.ID, newest.ID != id
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
