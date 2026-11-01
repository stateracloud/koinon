// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics of the DBOS runtime (ADR 0085 decision 11, ADR 0023).
//
//	koinon_durable_workflows_pending{version}  workflows PENDING, ENQUEUED or DELAYED, by the
//	                                           ApplicationVersion they wait for
//	koinon_durable_executors{version}          executors whose heartbeat is fresh, by version
//
// Both are read from the database, so every replica reports the same numbers: the gauges are
// idempotent across replicas and no election is needed (a leader-only gauge would go absent while
// leadership moves, and a dead leader's series would linger). Alerts and dashboards aggregate with
// max by (version), never sum. The read is cached for metricsCacheTTL, so a scrape is not a query.
//
// The version label is bounded: the integer WorkflowVersions are few (one per incompatible
// change), and every other value (the legacy release versions such as 0.1.0-rc.N) is folded into
// LegacyVersionLabel. No label carries a workflow, org, agent or run ID.

// LegacyVersionLabel is the version label of workflows whose ApplicationVersion is not an integer.
const LegacyVersionLabel = "legacy"

const (
	metricsCacheTTL = 15 * time.Second
	metricsTimeout  = 5 * time.Second
)

// versionLabel maps an ApplicationVersion to its label value.
func versionLabel(v string) string {
	if _, err := strconv.ParseUint(v, 10, 64); err != nil {
		return LegacyVersionLabel
	}
	return v
}

// counts are the two gauges' values, by version label.
type counts struct {
	pending   map[string]float64
	executors map[string]float64
}

// source reads the counts. The Runtime's reads the database.
type source func(ctx context.Context) (counts, error)

type collector struct {
	src     source
	current string // always reported, so "0" is a series and the alert can tell absent from zero
	ttl     time.Duration

	pendingDesc   *prometheus.Desc
	executorsDesc *prometheus.Desc
	errorsDesc    *prometheus.Desc

	mu      sync.Mutex
	at      time.Time
	cached  counts
	failing bool
}

func newCollector(src source, current string, ttl time.Duration) *collector {
	return &collector{
		src: src, current: versionLabel(current), ttl: ttl,
		pendingDesc: prometheus.NewDesc("koinon_durable_workflows_pending",
			"Durable workflows pending, enqueued or delayed, by the workflow version they wait for. Every replica reports the same value: aggregate with max by (version).",
			[]string{"version"}, nil),
		executorsDesc: prometheus.NewDesc("koinon_durable_executors",
			"Durable executors (koinon-api pods) with a fresh heartbeat, by workflow version. Every replica reports the same value: aggregate with max by (version).",
			[]string{"version"}, nil),
		errorsDesc: prometheus.NewDesc("koinon_durable_metrics_read_failing",
			"1 while this replica cannot read the durable workflow counts from the database, and the other two metrics are missing; 0 otherwise.",
			nil, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pendingDesc
	ch <- c.executorsDesc
	ch <- c.errorsDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= c.ttl {
		ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
		got, err := c.src(ctx)
		cancel()
		c.at = time.Now()
		c.failing = err != nil
		if err == nil {
			c.cached = got
		}
	}
	failing := 0.0
	if c.failing {
		failing = 1
	}
	ch <- prometheus.MustNewConstMetric(c.errorsDesc, prometheus.GaugeValue, failing)
	if c.failing {
		return
	}
	emit := func(desc *prometheus.Desc, m map[string]float64) {
		if _, ok := m[c.current]; !ok {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 0, c.current)
		}
		for v, n := range m {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, n, v)
		}
	}
	emit(c.pendingDesc, c.cached.pending)
	emit(c.executorsDesc, c.cached.executors)
}

// Collector returns the Prometheus collector of this runtime's durable metrics. Register it on the
// component's registry. Before Launch, and after Shutdown, it reports the read as failing.
func (r *Runtime) Collector() prometheus.Collector {
	return newCollector(r.readCounts, r.version(), metricsCacheTTL)
}

func (r *Runtime) readCounts(ctx context.Context) (counts, error) {
	r.mu.Lock()
	pool := r.pool
	r.mu.Unlock()
	if pool == nil {
		return counts{}, fmt.Errorf("durable: metrics before Launch")
	}
	return queryCounts(ctx, pool, r.staleAfter())
}

func queryCounts(ctx context.Context, pool *pgxpool.Pool, staleAfter time.Duration) (counts, error) {
	out := counts{pending: map[string]float64{}, executors: map[string]float64{}}
	add := func(rows pgx.Rows, into map[string]float64) error {
		defer rows.Close()
		for rows.Next() {
			var v string
			var n int64
			if err := rows.Scan(&v, &n); err != nil {
				return err
			}
			into[versionLabel(v)] += float64(n)
		}
		return rows.Err()
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT application_version, count(*) FROM %s
WHERE status IN ('PENDING', 'ENQUEUED', 'DELAYED') GROUP BY application_version`,
		pgx.Identifier{Schema, "workflow_status"}.Sanitize()))
	if err != nil {
		return counts{}, fmt.Errorf("durable: count pending workflows: %w", err)
	}
	if err := add(rows, out.pending); err != nil {
		return counts{}, fmt.Errorf("durable: count pending workflows: %w", err)
	}
	rows, err = pool.Query(ctx, fmt.Sprintf(`SELECT application_version, count(*) FROM %s
WHERE last_heartbeat >= now() - make_interval(secs => $1) GROUP BY application_version`, executorsTable()),
		staleAfter.Seconds())
	if err != nil {
		return counts{}, fmt.Errorf("durable: count executors: %w", err)
	}
	if err := add(rows, out.executors); err != nil {
		return counts{}, fmt.Errorf("durable: count executors: %w", err)
	}
	return out, nil
}
