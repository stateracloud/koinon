// SPDX-License-Identifier: Apache-2.0

// Command dashboardmetrics keeps the chart's Grafana dashboards in step with the code (issue 1078,
// ADR 0023). For each dashboard JSON file it checks:
//
//   - every metric name in a panel query (a name that starts with `koinon_`) is registered in the Go
//     code as `Name: "<metric>"` (a histogram series _bucket, _sum or _count counts as its base name),
//     so a renamed metric breaks CI and not the dashboard;
//   - no query groups or filters by a label that carries an org, agent, user, ticket or run ID.
//
// Usage: dashboardmetrics SRC_ROOT DASHBOARD.json... Exit codes: 0 clean, 1 findings, 2 usage or read
// error (ADR 0118). hack/dashboard-metrics-check.sh is the wrapper.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	metricRe   = regexp.MustCompile(`\bkoinon_[a-z0-9_]+`)
	registerRe = regexp.MustCompile(`Name:\s*"(koinon_[a-z0-9_]+)"`)
	groupingRe = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)
	matcherRe  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:=~|!~|!=|=)\s*"`)
	suffixes   = []string{"_bucket", "_sum", "_count"}
	// forbidden are the labels ADR 0023 keeps out of every metric.
	forbidden = []string{
		"org", "org_id", "organization", "agent", "agent_id", "user", "user_id", "ticket", "ticket_id",
		"run", "run_id", "session", "session_id",
	}
)

func main() {
	os.Exit(func() int {
		code, err := run(os.Args[1:], os.Stdout, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dashboardmetrics:", err)
			return 2
		}
		return code
	}())
}

func run(args []string, out, errOut io.Writer) (int, error) {
	if len(args) < 2 {
		return 0, fmt.Errorf("usage: dashboardmetrics SRC_ROOT DASHBOARD.json ...(one or more)")
	}
	registered, err := registeredMetrics(args[0])
	if err != nil {
		return 0, err
	}
	findings := 0
	for _, file := range args[1:] {
		raw, err := os.ReadFile(file) //nolint:gosec // the operator names the dashboards
		if err != nil {
			return 0, err
		}
		exprs, err := queries(raw)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", file, err)
		}
		if len(exprs) == 0 {
			_, _ = fmt.Fprintf(errOut, "%s: no panel query\n", file)
			findings++
		}
		for _, expr := range exprs {
			for _, msg := range check(expr, registered) {
				_, _ = fmt.Fprintf(errOut, "%s: %s\n", file, msg)
				findings++
			}
		}
	}
	if findings > 0 {
		return 1, nil
	}
	_, _ = fmt.Fprintln(out, "dashboardmetrics: ok")
	return 0, nil
}

// registeredMetrics returns the metric names the Go code under root registers (tests excluded).
func registeredMetrics(root string) (map[string]bool, error) {
	names := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // root is the operator argument
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p) //nolint:gosec // p comes from the walk of the operator root
		if err != nil {
			return err
		}
		for _, m := range registerRe.FindAllSubmatch(src, -1) {
			names[string(m[1])] = true
		}
		return nil
	})
	return names, err
}

type panel struct {
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
	Panels []panel `json:"panels"`
}

// queries returns every panel query of a dashboard, rows with nested panels included.
func queries(raw []byte) ([]string, error) {
	var d panel
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var out []string
	var walk func(ps []panel)
	walk = func(ps []panel) {
		for _, p := range ps {
			for _, t := range p.Targets {
				if t.Expr != "" {
					out = append(out, t.Expr)
				}
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out, nil
}

// check returns the findings of one query.
func check(expr string, registered map[string]bool) []string {
	var msgs []string
	for _, name := range metricRe.FindAllString(expr, -1) {
		if !known(name, registered) {
			msgs = append(msgs, fmt.Sprintf("metric %s is not registered in the code (renamed or removed?): %s", name, expr))
		}
	}
	var labels []string
	for _, m := range groupingRe.FindAllStringSubmatch(expr, -1) {
		for _, l := range strings.Split(m[1], ",") {
			labels = append(labels, strings.TrimSpace(l))
		}
	}
	for _, m := range matcherRe.FindAllStringSubmatch(expr, -1) {
		labels = append(labels, m[1])
	}
	for _, l := range labels {
		if slices.Contains(forbidden, l) {
			msgs = append(msgs, fmt.Sprintf("label %q is not a fixed label (ADR 0023): %s", l, expr))
		}
	}
	return msgs
}

func known(name string, registered map[string]bool) bool {
	if registered[name] {
		return true
	}
	for _, s := range suffixes {
		if base, ok := strings.CutSuffix(name, s); ok && registered[base] {
			return true
		}
	}
	return false
}
