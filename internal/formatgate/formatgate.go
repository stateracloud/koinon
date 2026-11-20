// SPDX-License-Identifier: Apache-2.0

// Package formatgate is the one place that says whether every running component can read a stored format
// (ADR 0097 decision 12.5 step 1). A writer of a new format asks Gate.Ready before it writes; a reader
// reports what its code reads with Reporter.
//
// To add a later format change:
//  1. Declare a Format below and name the components that must read it (Readers).
//  2. Add it to Formats. Every component binary already reports Reads(its component), so a release
//     that holds the reader code reports the new name with no other change.
//  3. Make the writer ask Gate.Ready(ctx, thatFormat) and fall back to the old format on ErrNotReady.
//
// The gate fails closed: no expectation for a reader component, a missing or stale report, a pod that
// does not list the format, or fewer reports than expected replicas all keep it shut. A component that
// is newer than the release (it lists more formats) passes. A pod from a release before this package
// writes no report, so the replica count is what shows it: the gate opens only when the expected number
// of pods report the format.
package formatgate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Component names a koinon binary.
type Component string

// The components that read stored formats.
const (
	API        Component = "api"
	Reconciler Component = "reconciler"
	Gateway    Component = "gateway"
)

// Format is a stored format that more than one component must read before any writer may use it.
type Format struct {
	Name    string
	Readers []Component
}

// MCPCredentialV2 is the sealed MCP credential (secretstore envelope v2): koinon-api writes it, the
// reconciler copies it and koinon-gateway opens it.
var MCPCredentialV2 = Format{Name: "mcp-credential-v2", Readers: []Component{API, Reconciler, Gateway}}

// Formats is every format a component binary reports. Add a new Format here.
var Formats = []Format{MCPCredentialV2}

// Reads is the names of the formats the code of component c reads.
func Reads(c Component) []string {
	var out []string
	for _, f := range Formats {
		if slices.Contains(f.Readers, c) {
			out = append(out, f.Name)
		}
	}
	return out
}

// DB is the part of a pgx pool this package uses.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DefaultEvery is how often a component reports, and DefaultTTL how long a report counts.
const (
	DefaultEvery = 15 * time.Second
	DefaultTTL   = 60 * time.Second
)

// Reporter writes the row of one process.
type Reporter struct {
	DB        DB
	Component Component
	Instance  string // the pod name; empty: the host name
	Reads     []string
	Every     time.Duration
	Log       func(msg string, err error)
}

// NewReporter reports what component c reads.
func NewReporter(db DB, c Component) Reporter {
	host, _ := os.Hostname()
	return Reporter{DB: db, Component: c, Instance: host, Reads: Reads(c)}
}

// Report writes the row once.
func (r Reporter) Report(ctx context.Context) error {
	if r.Instance == "" {
		return errors.New("formatgate: no instance name")
	}
	_, err := r.DB.Exec(ctx, `INSERT INTO component_reports (component, instance, reads, reported_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (component, instance) DO UPDATE SET reads = EXCLUDED.reads, reported_at = now()`,
		string(r.Component), r.Instance, append([]string{}, r.Reads...))
	return err
}

// Run reports now and then on a timer until ctx ends. A failed report is logged, never fatal: the
// gate stays shut while reports are missing.
func (r Reporter) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = DefaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Report(ctx); err != nil && ctx.Err() == nil && r.Log != nil {
			r.Log("formatgate: report failed", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ErrNotReady is what Gate.Ready returns while a writer must keep to the old format.
var ErrNotReady = errors.New("formatgate: not every component reads the format yet")

// Gate decides whether a format may be written.
type Gate struct {
	DB  DB
	TTL time.Duration
	// Expect is the number of pods of each component the chart runs. A reader component with no entry
	// keeps the gate shut.
	Expect map[Component]int
}

// ParseExpect reads "api=2,reconciler=1,gateway=2".
func ParseExpect(s string) (map[Component]int, error) {
	out := map[Component]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, n, ok := strings.Cut(part, "=")
		v, err := strconv.Atoi(strings.TrimSpace(n))
		if !ok || err != nil || v < 1 || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("formatgate: bad expectation %q, want component=count", part)
		}
		out[Component(strings.TrimSpace(name))] = v
	}
	return out, nil
}

// Ready returns nil when every reader of f has the expected number of fresh reports that all list f.
// Otherwise it returns an error that wraps ErrNotReady and names the first component that is short.
func (g Gate) Ready(ctx context.Context, f Format) error {
	ttl := g.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	rows, err := g.DB.Query(ctx, `SELECT component,
			count(*) FILTER (WHERE $2 = ANY (reads)),
			count(*) FILTER (WHERE NOT ($2 = ANY (reads)))
		FROM component_reports
		WHERE reported_at > now() - make_interval(secs => $1)
		GROUP BY component`, ttl.Seconds(), f.Name)
	if err != nil {
		return fmt.Errorf("formatgate: read reports: %w", err)
	}
	defer rows.Close()
	type count struct{ reading, lacking int }
	got := map[Component]count{}
	for rows.Next() {
		var c string
		var n count
		if err := rows.Scan(&c, &n.reading, &n.lacking); err != nil {
			return fmt.Errorf("formatgate: read reports: %w", err)
		}
		got[Component(c)] = n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("formatgate: read reports: %w", err)
	}
	for _, c := range f.Readers {
		want := g.Expect[c]
		switch n := got[c]; {
		case want < 1:
			return fmt.Errorf("%w: no replica count is set for %s", ErrNotReady, c)
		case n.lacking > 0:
			return fmt.Errorf("%w: %d %s pod(s) do not read %s", ErrNotReady, n.lacking, c, f.Name)
		case n.reading < want:
			return fmt.Errorf("%w: %d of %d %s pod(s) report %s", ErrNotReady, n.reading, want, c, f.Name)
		}
	}
	return nil
}
