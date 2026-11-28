// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/execution"
)

// Results of a repoint, the values of koinon_reconciler_sandbox_repoints_total{result} (a fixed set, ADR 0023).
const (
	repointDone         = "repointed"
	repointRefused      = "refused"
	repointResumeFailed = "resume_failed"
)

// repoint gives a sandbox its new spec in place, and keeps its home (issue #1482, decision D2 of the
// 2026-11-27 maintainer ruling). A driver that can repoint (execution.Repointer, Substrate) suspends the
// actor with a durable snapshot, points it at the new template and resumes it: 4 to 12 s, against a
// delete and a restore of the whole home from kopia. The driver, not the reconciler, enforces what keeps
// the home: the same container name, volumes, mounts, storage location and tier.
//
// It reports done when the sandbox was handled and the caller must stop. It reports not done, with no
// error, when the caller goes on with the delete and restore path: the backend cannot repoint, the sandbox
// is in no phase to repoint, the sandbox needs a new identity anyway (unnamedActor), or the repoint was
// refused (a tier change, a template change that would empty the home, or a refusal of Substrate). A
// refusal changes nothing, so the delete and restore path runs against the unchanged sandbox.
//
// The caller holds a recreate slot (takeRecreate): a fleet-wide image change repoints at the rate of the
// cap, as the delete path would. The slot ends here when the repoint was tried and not refused.
func (r *Reconciler) repoint(ctx context.Context, w *work, ws string, spec execution.SandboxSpec, sb execution.Sandbox, hash string) (done bool, err error) {
	rp, ok := execution.RepointerOf(r.backend)
	if !ok || !execution.IdentifiesSpecs(r.backend) || r.unnamedActor(w) {
		return false, nil
	}
	// A sandbox that is suspended or runs. Provisioning, deleting and crashed sandboxes go the old way.
	if sb.Phase != execution.PhaseReady && !sb.Phase.IsStopped() {
		return false, nil
	}
	if err := r.snapshotClock(ctx, w); err != nil { // the repoint takes snapshots
		return true, err
	}
	res, err := rp.RepointSandbox(ctx, ws, sb.Name, spec)
	key := agentKey{w.org.ID, w.agent.ID}
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrRepointRefused):
		r.m.SandboxRepoints.WithLabelValues(repointRefused).Inc()
		r.log.Warn("the sandbox cannot be repointed; recreating it from the home checkpoint", append(w.logAttrs(), "err", err)...)
		return false, nil
	case errors.Is(err, execution.ErrRepointResume):
		// The sandbox names the new template and holds its home. It must not be deleted. The next pass
		// reads its state, finds the spec equal, and starts or stops it as usual.
		r.recreateStarted(key)
		r.m.SandboxRepoints.WithLabelValues(repointResumeFailed).Inc()
		r.log.Warn("the sandbox was repointed and did not resume", append(w.logAttrs(), "err", err)...)
		return true, r.record(ctx, w, sb, hash, "repointed to the new spec; the resume did not work yet")
	default:
		return true, err
	}
	r.recreateStarted(key)
	r.m.SandboxRepoints.WithLabelValues(repointDone).Inc()
	r.log.Info("repointed a sandbox to a changed spec, the home kept", append(w.logAttrs(), "spec", res.SpecID)...)
	if err := audit.RecordAfter(ctx, r.db, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-repoint"), Action: "sandbox.repoint", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: map[string]any{"sandbox": sb.Name, "from_spec": sb.SpecID, "to_spec": res.SpecID},
	}); err != nil {
		return true, err
	}
	return true, r.record(ctx, w, res, hash, "repointed to a changed spec")
}
