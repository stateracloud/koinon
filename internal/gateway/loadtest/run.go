// SPDX-License-Identifier: Apache-2.0

// Package loadtest is the load harness of the gateway (issue 280). It drives a gateway with many
// concurrent callers and reports latency percentiles, throughput and the error rate. A driver's test
// (build tag "load") starts its gateway, calls the scenarios of this package and prints the [Result]s.
//
// The load tests run in the cloud only: never in a required CI job, never from `make test`.
// Run them with `make load-gateway` (docs/dev/load-tests.md).
package loadtest

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Op is one call of a worker. It returns an error when the call failed.
type Op func(ctx context.Context) error

// Worker is one caller. Setup makes its Op (for example it opens an MCP session); the Op reconnects
// by itself after an error, as a client does. Close ends the worker. Close may be nil.
type Worker struct {
	Op    Op
	Close func()
}

// Spec describes one measured run: Concurrency workers each call in a loop (closed loop: a worker
// sends its next call when the last one is done) for Duration, after a Warmup that is not counted.
type Spec struct {
	// Name is the scenario's name in the report.
	Name string
	// Concurrency is the number of workers.
	Concurrency int
	// Duration is the length of the measured part.
	Duration time.Duration
	// Warmup runs first and is not counted.
	Warmup time.Duration
	// NewWorker makes worker i.
	NewWorker func(ctx context.Context, i int) (Worker, error)
	// During, when set, runs once at the given offset into the measured part, in its own goroutine
	// (a replica restart or a drain, for example). The run waits for it to return.
	During      func(ctx context.Context) error
	DuringAfter time.Duration
}

// Result is what one run measured. Latencies are those of the calls that succeeded.
type Result struct {
	Scenario    string        `json:"scenario"`
	Driver      string        `json:"driver"`
	Concurrency int           `json:"concurrency"`
	Duration    time.Duration `json:"durationNs"`
	Calls       int64         `json:"calls"`
	Errors      int64         `json:"errors"`
	// Throughput is successful calls per second.
	Throughput float64       `json:"throughputPerSecond"`
	Min        time.Duration `json:"minNs"`
	P50        time.Duration `json:"p50Ns"`
	P95        time.Duration `json:"p95Ns"`
	P99        time.Duration `json:"p99Ns"`
	Max        time.Duration `json:"maxNs"`
	// FirstError and LastError are the offsets into the measured part of the first and last failed
	// call; both are zero when no call failed. Their difference is the outage window of a restart.
	FirstError time.Duration `json:"firstErrorNs,omitempty"`
	LastError  time.Duration `json:"lastErrorNs,omitempty"`
	// ErrorKinds counts the failed calls by the first words of the error.
	ErrorKinds map[string]int64 `json:"errorKinds,omitempty"`
	// Notes holds numbers a scenario measures besides these (progress notifications received).
	Notes map[string]float64 `json:"notes,omitempty"`
}

// ErrorRate is the share of calls that failed, 0 to 1.
func (r Result) ErrorRate() float64 {
	if r.Calls+r.Errors == 0 {
		return 0
	}
	return float64(r.Errors) / float64(r.Calls+r.Errors)
}

// Percentile is the nearest-rank percentile (p in 0 to 100) of sorted, which must be ascending.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p/100*float64(len(sorted)) + 0.999999)
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// Run executes spec and returns what it measured. A worker that cannot be made stops the run.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.Concurrency <= 0 || spec.Duration <= 0 || spec.NewWorker == nil {
		return Result{}, fmt.Errorf("loadtest: %q needs a concurrency, a duration and a worker", spec.Name)
	}
	workers := make([]Worker, 0, spec.Concurrency)
	defer func() {
		for _, w := range workers {
			if w.Close != nil {
				w.Close()
			}
		}
	}()
	for i := range spec.Concurrency {
		w, err := spec.NewWorker(ctx, i)
		if err != nil {
			return Result{}, fmt.Errorf("loadtest: %q worker %d: %w", spec.Name, i, err)
		}
		workers = append(workers, w)
	}

	start := time.Now().Add(spec.Warmup)
	end := start.Add(spec.Duration)
	type sample struct {
		lat   []time.Duration
		errs  int64
		first time.Duration
		last  time.Duration
		kinds map[string]int64
	}
	samples := make([]sample, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &samples[i]
			s.kinds = map[string]int64{}
			for ctx.Err() == nil {
				t0 := time.Now()
				if !t0.Before(end) {
					return
				}
				err := w.Op(ctx)
				t1 := time.Now()
				if t0.Before(start) {
					continue // warmup
				}
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					off := t1.Sub(start)
					if s.errs == 0 {
						s.first = off
					}
					s.last = off
					s.errs++
					s.kinds[errorKind(err)]++
					// A failed call can return at once (connection refused). Pause a little so a
					// down gateway is not a busy loop that skews the other workers.
					select {
					case <-ctx.Done():
					case <-time.After(10 * time.Millisecond):
					}
					continue
				}
				s.lat = append(s.lat, t1.Sub(t0))
			}
		}()
	}
	var duringErr atomic.Value
	if spec.During != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(start.Add(spec.DuringAfter))):
			}
			if err := spec.During(ctx); err != nil {
				duringErr.Store(err)
			}
		}()
	}
	wg.Wait()

	res := Result{Scenario: spec.Name, Concurrency: spec.Concurrency, Duration: spec.Duration, ErrorKinds: map[string]int64{}}
	var all []time.Duration
	for _, s := range samples {
		all = append(all, s.lat...)
		res.Errors += s.errs
		for k, n := range s.kinds {
			res.ErrorKinds[k] += n
		}
		if s.errs > 0 {
			if res.LastError == 0 || s.first < res.FirstError {
				res.FirstError = s.first
			}
			res.LastError = max(res.LastError, s.last)
		}
	}
	if len(res.ErrorKinds) == 0 {
		res.ErrorKinds = nil
	}
	slices.Sort(all)
	res.Calls = int64(len(all))
	res.Throughput = float64(len(all)) / spec.Duration.Seconds()
	if len(all) > 0 {
		res.Min, res.Max = all[0], all[len(all)-1]
		res.P50, res.P95, res.P99 = Percentile(all, 50), Percentile(all, 95), Percentile(all, 99)
	}
	if err, ok := duringErr.Load().(error); ok {
		return res, fmt.Errorf("loadtest: %q disturbance: %w", spec.Name, err)
	}
	return res, ctx.Err()
}

// errorKind shortens an error to a key that groups alike errors.
func errorKind(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if i := strings.IndexAny(s, "0123456789"); i > 0 && i < 48 {
		s = strings.TrimSpace(s[:i])
	}
	return cmp.Or(s[:min(len(s), 60)], "error")
}

// Report writes results as a Markdown table, the form the PR body and docs/dev/load-tests.md use.
func Report(w io.Writer, results []Result) error {
	var b strings.Builder
	b.WriteString("| driver | scenario | conc. | calls | errors | err % | calls/s | p50 | p95 | p99 | max |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %.2f | %.0f | %s | %s | %s | %s |\n",
			r.Driver, r.Scenario, r.Concurrency, r.Calls, r.Errors, 100*r.ErrorRate(), r.Throughput,
			ms(r.P50), ms(r.P95), ms(r.P99), ms(r.Max))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d)/float64(time.Millisecond)) }
