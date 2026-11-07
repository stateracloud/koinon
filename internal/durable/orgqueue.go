// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Per-org queues (plan item DB2, ADR 0066, old issue 146 2026-10-26). Trigger firings, webhook processing and
// task creation run through one DBOS queue that is partitioned by org, so one org's burst waits in
// its own partition and does not delay another org's work. Run concurrency is not here: it is
// enforced where runs are claimed, in Postgres (run_claimable, migration 00023).
//
// DBOS applies one partition limit to every partition of a queue. An org whose limits differ from
// the defaults therefore gets a queue of its own for that limit pair (profile queue), still
// partitioned by org. DBOS stores queue configuration in its system tables and its queue runners
// reload the list of queues, so a profile queue registered at enqueue time is picked up by every
// replica, and rate limits count workflow starts in Postgres, so they hold across restarts.

// Defaults for the org-partitioned queue (recorded in ADR 0066's DB2 amendment).
const (
	// OrgQueueName is the queue every org without custom limits uses.
	OrgQueueName = "koinon.org-work"
	// DefaultOrgConcurrency is how many items of one org run at once.
	DefaultOrgConcurrency = 4
	// DefaultOrgRatePerWindow is how many items of one org may start per DefaultOrgRateWindow.
	DefaultOrgRatePerWindow = 60
	// DefaultOrgRateWindow is the rate-limit window.
	DefaultOrgRateWindow = time.Minute
)

// OrgLimits are one org's limits on the org-partitioned queue. A zero field means the default.
type OrgLimits struct {
	// Concurrency is how many items run at once.
	Concurrency int
	// RatePerWindow is how many items may start per OrgQueueConfig.RateWindow.
	RatePerWindow int
}

// LimitsFunc returns the org's limits, for example from its org settings (internal/orgsettings).
// It is called on each enqueue, so changes apply to work enqueued afterwards.
type LimitsFunc func(ctx context.Context, org string) (OrgLimits, error)

// OrgQueueConfig configures OrgWork.
type OrgQueueConfig struct {
	// Defaults fill every zero limit; zero values use the package defaults. An org whose limits
	// equal Defaults stays on the main queue, so when Limits comes from org settings, set Defaults
	// to the settings defaults (orgsettings.QueueDefaults).
	Defaults OrgLimits
	// RateWindow is the rate-limit window; zero is DefaultOrgRateWindow. It is part of a profile
	// queue's name. The org settings store a rate per minute (orgsettings.QueueLimits), so
	// production leaves this at one minute; a different window changes what that rate means.
	RateWindow time.Duration
	// Limits resolves an org's limits; nil gives every org the defaults.
	Limits LimitsFunc
	// PollingInterval is the queue's base polling interval; zero is DBOS's default.
	PollingInterval time.Duration
}

// OrgWork enqueues org-scoped work on the org-partitioned queue. Create it with NewOrgWork before
// Launch; it registers the default queue on the runtime.
type OrgWork struct {
	rt  *Runtime
	cfg OrgQueueConfig

	mu    sync.Mutex
	known map[string]bool
}

// NewOrgWork registers the org-partitioned default queue on rt and returns the enqueue API for
// the trigger (TR2), webhook (TR3) and task-creation paths.
func NewOrgWork(rt *Runtime, cfg OrgQueueConfig) (*OrgWork, error) {
	if cfg.Defaults.Concurrency <= 0 {
		cfg.Defaults.Concurrency = DefaultOrgConcurrency
	}
	if cfg.Defaults.RatePerWindow <= 0 {
		cfg.Defaults.RatePerWindow = DefaultOrgRatePerWindow
	}
	if cfg.RateWindow <= 0 {
		cfg.RateWindow = DefaultOrgRateWindow
	}
	w := &OrgWork{rt: rt, cfg: cfg, known: map[string]bool{}}
	if err := rt.Register(func(c dbos.Context) error {
		_, err := w.ensureQueue(c, w.cfg.Defaults)
		return err
	}); err != nil {
		return nil, err
	}
	return w, nil
}

// Runtime is the runtime the work is enqueued on.
func (w *OrgWork) Runtime() *Runtime { return w.rt }

func (w *OrgWork) resolve(ctx context.Context, org string) (OrgLimits, error) {
	l := OrgLimits{}
	if w.cfg.Limits != nil {
		var err error
		if l, err = w.cfg.Limits(ctx, org); err != nil {
			return l, fmt.Errorf("durable: limits of org %s: %w", org, err)
		}
	}
	if l.Concurrency <= 0 {
		l.Concurrency = w.cfg.Defaults.Concurrency
	}
	if l.RatePerWindow <= 0 {
		l.RatePerWindow = w.cfg.Defaults.RatePerWindow
	}
	return l, nil
}

// queueName is OrgQueueName for the defaults and a profile name for any other pair.
func (w *OrgWork) queueName(l OrgLimits) string {
	if l == w.cfg.Defaults {
		return OrgQueueName
	}
	return fmt.Sprintf("%s/c%d/r%dp%d", OrgQueueName, l.Concurrency, l.RatePerWindow, int(w.cfg.RateWindow/time.Second))
}

func (w *OrgWork) ensureQueue(c dbos.Context, l OrgLimits) (string, error) {
	name := w.queueName(l)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.known[name] {
		return name, nil
	}
	opts := []dbos.QueueOption{
		dbos.WithPartitionConcurrency(l.Concurrency),
		dbos.WithPartitionRateLimiter(&dbos.RateLimiter{Limit: l.RatePerWindow, Period: w.cfg.RateWindow}),
	}
	if w.cfg.PollingInterval > 0 {
		opts = append(opts, dbos.WithQueueBasePollingInterval(w.cfg.PollingInterval))
	}
	if _, err := dbos.RegisterQueue(c, name, opts...); err != nil {
		return "", fmt.Errorf("durable: register queue %s: %w", name, err)
	}
	w.known[name] = true
	return name, nil
}

// EnqueueForOrg enqueues the registered workflow workflowName with input on the org's partition
// of the org queue, and returns its handle. R is the workflow's result type. Pass DBOS enqueue
// options (a workflow ID, a delay, a priority) in opts; the queue and partition are set here.
// The runtime must be launched.
func EnqueueForOrg[R, P any](ctx context.Context, w *OrgWork, org, workflowName string, input P, opts ...dbos.EnqueueOption) (dbos.WorkflowHandle[R], error) {
	if org == "" {
		return nil, errors.New("durable: EnqueueForOrg needs an org ID")
	}
	dctx := w.rt.Context()
	if dctx == nil {
		return nil, errors.New("durable: EnqueueForOrg before Launch")
	}
	l, err := w.resolve(ctx, org)
	if err != nil {
		return nil, err
	}
	name, err := w.ensureQueue(dctx, l)
	if err != nil {
		return nil, err
	}
	all := append(slices.Clone(opts), dbos.WithEnqueueQueuePartitionKey(org))
	return dbos.Enqueue[R](dctx, name, workflowName, input, all...)
}
