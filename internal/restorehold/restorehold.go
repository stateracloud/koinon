// SPDX-License-Identifier: Apache-2.0

// Package restorehold is the restore-hold mode of an install (issue #462): the three binaries start,
// serve their health and metrics listeners and answer reads, but they do not act outward. koinon-api
// runs no DBOS runtime (so no schedule fires, no pending workflow is recovered and no queue drains)
// and no code-host scope sync; koinon-agentd claims no run and takes over none; koinon-reconciler
// converges no sandbox and settles no spend.
//
// The mode is one setting for the install, the flag --restore-hold (KOINON_RESTORE_HOLD) that the
// chart sets on all three Deployments from restoreHold. It is read at start, so a release is a
// rollout: set the value back and upgrade. Postgres's recovery_target_action=pause is the prior art
// for starting a restored system and holding it before it acts; a flag instead of a row in the
// database means the hold also covers a restore of a dump taken before the operator set anything,
// and a component that cannot read the database still holds.
package restorehold

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
)

// FlagName is the flag (and, with the KOINON_ prefix, the environment variable) of all three binaries.
const FlagName = "restore-hold"

// Usage is the flag's help text.
const Usage = "restore-hold mode: start and serve health and reads, but act on nothing until an operator releases the hold " +
	"(no schedule fires, no run is claimed or taken over, no sandbox is converged, no code-host scope is read); " +
	"release it by starting without the flag (the chart value restoreHold)"

// Flag registers --restore-hold on fs and returns the variable it fills.
func Flag(fs *pflag.FlagSet, into *bool) {
	fs.BoolVar(into, FlagName, false, Usage)
}

// Metric registers koinon_restore_hold on reg: 1 while this process holds, 0 otherwise. A fixed
// label-free gauge (ADR 0023); the chart alert KoinonRestoreHoldOn and the backup-restore guide read it
// to see that the hold is on and that a release took.
func Metric(reg prometheus.Registerer, held bool) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "koinon_restore_hold",
		Help: "1 while the process runs in restore-hold mode and acts on nothing, else 0.",
	})
	if held {
		g.Set(1)
	}
	reg.MustRegister(g)
}

// Wait logs that component holds and blocks until ctx ends. It is what a component runs in place
// of its work loop. It returns nil: a hold that ends with the process is not an error.
func Wait(ctx context.Context, log *slog.Logger, component string) error {
	log.Warn("restore-hold: "+component+" acts on nothing until the hold is released (restoreHold=false in the chart, then a rollout)",
		"component", component)
	<-ctx.Done()
	return nil
}

// Message is what an admin sees when a held install refuses a call that would reach outward.
const Message = "install is held for restore; release the hold first (restoreHold=false in the chart, then a rollout; " +
	"see the \"Back up and restore\" guide)"

// ErrHeld is what a guarded service returns while the install is held. Both API surfaces map it to
// a failed precondition (Connect FailedPrecondition, HTTP 409) with Message.
var ErrHeld = errors.New(Message)

// Guard is the one shared check of a held install. A service that reaches outward (a model call, a
// code-host read or write, an MCP dial, a trace export, an approved call's execution) holds a Guard
// and calls Err before it does so, so the hold covers every caller of the service and not one API
// surface. The nil Guard is an install that is not held.
type Guard struct{ held bool }

// NewGuard returns a Guard that refuses while held is true.
func NewGuard(held bool) *Guard { return &Guard{held: held} }

// Held says whether the install is held.
func (g *Guard) Held() bool { return g != nil && g.held }

// Err is nil when the install is not held and ErrHeld when it is.
func (g *Guard) Err() error {
	if g.Held() {
		return ErrHeld
	}
	return nil
}
