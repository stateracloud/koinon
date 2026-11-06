// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/providerusage"
)

// The tolerance of the daily reconciliation (ADR 0056, amendment 2026-11-06): a provider key's day
// is ok when the ledger is within CostTolerancePercent of the provider's reported cost, or within
// CostToleranceFloor of it, whichever is wider (a day of a few cents is not out of tolerance by a
// rounding of the provider's report). Direct providers are charged the catalog price, so the
// difference is the price list's error plus the usage the gateway could not see (cut streams).
const CostTolerancePercent = 2

// CostToleranceFloor is the absolute part of the tolerance: one cent.
var CostToleranceFloor = money.FromCents(1)

// WithinCostTolerance reports whether the ledger's figure is within the tolerance of the
// provider's.
func WithinCostTolerance(ledger, provider money.USD) bool {
	d := new(big.Int).Sub(big.NewInt(int64(ledger)), big.NewInt(int64(provider)))
	d.Abs(d)
	// |d| * 100 <= provider * percent, or |d| <= floor.
	if d.Cmp(big.NewInt(int64(CostToleranceFloor))) <= 0 {
		return true
	}
	lhs := new(big.Int).Mul(d, big.NewInt(100))
	rhs := new(big.Int).Mul(big.NewInt(int64(provider)), big.NewInt(CostTolerancePercent))
	return lhs.Cmp(rhs) <= 0
}

// Reconciliation outcomes: provider_cost_reconciliations.status and the fixed outcome label of
// koinon_reconciler_cost_reconciliations_total.
const (
	CostOK             = "ok"
	CostOutOfTolerance = "out_of_tolerance"
	CostError          = "error"
)

// CostReconciliationDays is how many complete UTC days back a reconciliation re-reads: the
// providers' cost reports fill in and are revised for a while after a day ends.
const CostReconciliationDays = 3

// costRecheck is how long a recorded day stands before it is read again; a failed read is retried
// sooner.
const (
	costRecheck      = 23 * time.Hour
	costRetryOnError = time.Hour
)

// costLockKey is the advisory lock that lets one replica reconcile at a time.
const costLockKey int64 = 0x6b6f696e6f6e4372 // "koinonCr"

// CostReconciler compares, once a day for each direct provider key with a cost API key, the
// ledger's cost of the key's entries for each UTC day with the provider's cost API for the key's
// workspace or project, and records the difference (provider_cost_reconciliations). It changes no
// entry: direct requests are charged the catalog price (ADR 0056, amendment 2026-11-06); the
// record shows how far that is from the bill. One replica runs a pass at a time.
type CostReconciler struct {
	DB *pgxpool.Pool
	// Keys opens the cost API keys (the secret store's keyring).
	Keys catalog.Opener
	// Sources reads the cost APIs per provider type; nil is providerusage.DefaultCosts.
	Sources providerusage.CostRegistry
	Log     *slog.Logger
	// Metrics counts the outcomes; nil records none.
	Metrics *CostMetrics
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Interval between passes; zero is an hour. A day is read again only after costRecheck.
	Interval time.Duration
}

// CostMetrics are the reconciliation's metrics: fixed labels only (ADR 0023), never an org or
// provider ID.
type CostMetrics struct {
	// Reconciliations counts recorded days by provider type and outcome.
	Reconciliations *prometheus.CounterVec
}

// NewCostMetrics creates the metrics and registers them with reg (nil: not registered).
func NewCostMetrics(reg prometheus.Registerer, types []string) *CostMetrics {
	m := &CostMetrics{Reconciliations: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_reconciler_cost_reconciliations_total",
		Help: "Days of a direct provider key reconciled against the provider's cost API, by provider type and outcome (ok, out_of_tolerance, error).",
	}, []string{"provider_type", "outcome"})}
	for _, t := range types {
		for _, o := range []string{CostOK, CostOutOfTolerance, CostError} {
			m.Reconciliations.WithLabelValues(t, o)
		}
	}
	if reg != nil {
		reg.MustRegister(m.Reconciliations)
	}
	return m
}

func (c *CostReconciler) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CostReconciler) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *CostReconciler) sources() providerusage.CostRegistry {
	if c.Sources != nil {
		return c.Sources
	}
	return providerusage.DefaultCosts(nil)
}

// Run reconciles every Interval until ctx ends.
func (c *CostReconciler) Run(ctx context.Context) {
	every := c.Interval
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := c.ReconcileDue(ctx); err != nil && ctx.Err() == nil {
			c.log().Warn("cost reconciliation: pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReconcileDue records every due day of every provider with a cost API key and returns how many it
// recorded. A pass that finds another replica's pass running does nothing.
func (c *CostReconciler) ReconcileDue(ctx context.Context) (int, error) {
	conn, err := c.DB.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", costLockKey).Scan(&locked); err != nil {
		return 0, fmt.Errorf("cost reconciliation lock: %w", err)
	}
	if !locked {
		return 0, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", costLockKey)
	}()

	q := store.New(c.DB)
	provs, err := q.ListCostReportProviders(ctx, c.sources().Types())
	if err != nil {
		return 0, fmt.Errorf("list providers with a cost key: %w", err)
	}
	now := c.now().UTC()
	today := now.Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -CostReconciliationDays)
	done := 0
	var errs []error
	for _, p := range provs {
		if ctx.Err() != nil {
			break
		}
		rows, err := q.ListProviderCostReconciliations(ctx, store.ListProviderCostReconciliationsParams{
			OrgID: p.OrgID, ProviderID: p.ID, Since: pgtype.Date{Time: first, Valid: true}, MaxRows: CostReconciliationDays + 1,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		seen := map[time.Time]store.ProviderCostReconciliation{}
		for _, r := range rows {
			seen[r.Day.Time.UTC()] = r
		}
		for day := first; day.Before(today); day = day.AddDate(0, 0, 1) {
			if r, ok := seen[day]; ok {
				wait := costRecheck
				if r.Status == CostError {
					wait = costRetryOnError
				}
				if now.Sub(r.CheckedAt.Time) < wait {
					continue
				}
			}
			if err := c.reconcile(ctx, q, p, day); err != nil {
				errs = append(errs, err)
				continue
			}
			done++
		}
	}
	return done, errors.Join(errs...)
}

// reconcile records one provider's day: the ledger's sum, and the provider's cost or why it could
// not be read.
func (c *CostReconciler) reconcile(ctx context.Context, q *store.Queries, p store.ListCostReportProvidersRow, day time.Time) error {
	led, err := q.ProviderLedgerForDay(ctx, store.ProviderLedgerForDayParams{
		OrgID: p.OrgID, ProviderID: p.ID,
		Since: pgtype.Timestamptz{Time: day, Valid: true}, Until: pgtype.Timestamptz{Time: day.Add(24 * time.Hour), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("sum the ledger: %w", err)
	}
	ledger, err := money.ParseNumeric(led.CostUsd)
	if err != nil {
		return err
	}
	row := store.UpsertProviderCostReconciliationParams{
		OrgID: p.OrgID, ProviderID: p.ID, Day: pgtype.Date{Time: day, Valid: true}, ProviderType: p.Type,
		LedgerUsd: ledger.Numeric(), Entries: led.Entries, CheckedAt: pgtype.Timestamptz{Time: c.now(), Valid: true},
	}
	provider, readErr := c.read(ctx, p, day)
	switch {
	case readErr != nil:
		row.Status = CostError
		msg := readErr.Error()
		if len(msg) > 1024 {
			msg = strings.ToValidUTF8(msg[:1024], "")
		}
		row.Error = pgtype.Text{String: msg, Valid: true}
		c.log().Warn("cost reconciliation: the provider's cost could not be read",
			"org", p.OrgID.String(), "provider", p.ID.String(), "provider_type", p.Type, "day", day.Format(time.DateOnly), "err", readErr)
	default:
		row.ProviderUsd = pgtype.Text{String: provider.Numeric(), Valid: true}
		row.DiffUsd = pgtype.Text{String: (ledger - provider).Numeric(), Valid: true}
		row.Status = CostOK
		if !WithinCostTolerance(ledger, provider) {
			row.Status = CostOutOfTolerance
			c.log().Warn("cost reconciliation: the ledger is out of tolerance of the provider's cost",
				"org", p.OrgID.String(), "provider", p.ID.String(), "provider_type", p.Type, "day", day.Format(time.DateOnly),
				"ledger_usd", ledger.String(), "provider_usd", provider.String())
		}
	}
	if _, err := q.UpsertProviderCostReconciliation(ctx, row); err != nil {
		return fmt.Errorf("record the reconciliation: %w", err)
	}
	if c.Metrics != nil {
		c.Metrics.Reconciliations.WithLabelValues(p.Type, row.Status).Inc()
	}
	return nil
}

// read asks the provider's cost API for the day.
func (c *CostReconciler) read(ctx context.Context, p store.ListCostReportProvidersRow, day time.Time) (money.USD, error) {
	src, ok := c.sources().For(p.Type, p.Url)
	if !ok {
		return 0, errors.New("no cost source serves the provider's URL any more")
	}
	key, err := catalog.OpenCostKey(c.Keys, p.OrgID, p.ID, p.CostApiKey)
	if err != nil {
		return 0, fmt.Errorf("open the cost API key: %w", err)
	}
	return src.DayCost(ctx, providerusage.CostProvider{AdminKey: key, Scope: p.CostScope}, day)
}
