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
// failure is retried on a backoff until it succeeds or ctx ends.
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

// once runs one attempt.
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
	if st.Released {
		r.log.Warn("restore released: every session was revoked and the removals the restore brought in were recorded",
			"sessions", st.RevokedSessions, "removals", len(plan), "accepted", r.accept)
	}
	return nil
}

// Try makes the first attempt, bounded by timeout, and reports whether it succeeded.
func (r *releaseAtStart) Try(ctx context.Context, timeout time.Duration) bool {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := r.once(c)
	if err != nil {
		r.log.Warn("restore release failed; retrying", "err", err)
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
		r.log.Warn("restore release failed; retrying", "err", err)
		d = min(d*2, time.Minute)
	}
}
