// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/installbinding"
)

// releaseAtStart is the release step of an unheld start (ADR 0110 decisions 1 and 2): it revokes every
// session and records restore.release once, whatever the importer does. It does not depend on an importer
// pass, so a Secrets List failure, an org lookup error or a lock that cannot be taken in the importer
// cannot leave a pending release unsettled. plan reads the removals the restore brings in (nil without
// Kubernetes access, where no importer runs). The step tries once before the caller serves, and a
// failure is retried on a backoff until it succeeds or ctx ends. Until a release that is pending has
// run, the Binding's session gate refuses every session and sign-in ([installbinding.Binding.HoldSessions]).
type releaseAtStart struct {
	binding *installbinding.Binding
	plan    func(context.Context) ([]installbinding.Removal, error)
	accept  bool
	log     *slog.Logger
	// backoff is the first retry delay; zero is 5 seconds. after is time.After in production.
	backoff time.Duration
	after   func(time.Duration) <-chan time.Time
}

func newReleaseAtStart(b *installbinding.Binding, pool *pgxpool.Pool, kube kubernetes.Interface, namespace string, accept bool) *releaseAtStart {
	r := &releaseAtStart{binding: b, accept: accept, log: slog.Default()}
	if kube != nil {
		r.plan = func(ctx context.Context) ([]installbinding.Removal, error) {
			return declarative.PlanRemovals(ctx, pool, kube, namespace)
		}
	}
	return r
}

// once runs one attempt. A success opens the session gate.
func (r *releaseAtStart) once(ctx context.Context) error {
	var plan []installbinding.Removal
	if r.plan != nil {
		p, err := r.plan(ctx)
		if err != nil {
			return err
		}
		plan = p
	}
	st, err := r.binding.Settle(ctx, plan, r.accept)
	if err != nil {
		return err
	}
	r.binding.HoldSessions(false)
	if st.Released {
		r.log.Warn("restore released: every session was revoked and the removals the restore brought in were recorded",
			"sessions", st.RevokedSessions, "removals", len(plan), "accepted", r.accept)
	}
	return nil
}

// failed keeps the session gate closed after a failed attempt unless the row shows that no release
// is pending (an install with nothing to release, or another replica ran the step). A failed read of
// the row keeps the gate closed: the gate fails closed (ADR 0110 decision 1).
func (r *releaseAtStart) failed(ctx context.Context, err error) {
	pending, perr := r.binding.ReleasePending(ctx)
	switch {
	case perr != nil:
		r.log.Warn("restore release failed and the binding row is unreadable; sessions and sign-ins stay refused, retrying", "err", err, "readErr", perr)
	case pending:
		r.log.Warn("restore release failed; sessions and sign-ins stay refused until it succeeds, retrying", "err", err)
	default:
		r.binding.HoldSessions(false)
		r.log.Warn("restore removal list not updated; retrying", "err", err)
	}
}

// Try closes the session gate, makes the first attempt (bounded by timeout) and reports whether it
// succeeded. The caller runs it before the listeners start, so no request sees an open gate before the
// release step has run.
func (r *releaseAtStart) Try(ctx context.Context, timeout time.Duration) bool {
	r.binding.HoldSessions(true)
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := r.once(c)
	if err != nil {
		r.failed(ctx, err)
	}
	return err == nil
}

// Retry repeats the attempt with a doubling backoff (capped at one minute) until it succeeds.
func (r *releaseAtStart) Retry(ctx context.Context) {
	d := r.backoff
	if d <= 0 {
		d = 5 * time.Second
	}
	after := r.after
	if after == nil {
		after = time.After
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(d):
		}
		err := r.once(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		r.failed(ctx, err)
		d = min(d*2, time.Minute)
	}
}
