// SPDX-License-Identifier: Apache-2.0

// Command testverdict decides whether a run of Ginkgo suites really ran (#507). Exit codes from
// `ginkgo` and `go test` are not enough: a suite that skips as a whole exits 0, and under
// `ginkgo -p` a suite whose processes exit before reporting prints "Test Suite Passed" and writes
// no report at all.
//
// It reads the Ginkgo JSON reports in a directory and gives every expected suite (a package
// directory) one verdict:
//
//	PASS     at least one spec passed and none failed
//	FAIL     a spec or a suite node failed
//	SKIPPED  no spec ran because a requirement is missing (testenv: "koinon:requires=<name>: ...")
//	NOT RUN  no report, or no spec ran without a declared requirement
//
// FAIL and NOT RUN exit 1. SKIPPED exits 0 unless KOINON_REQUIRE lists the requirement, so
// `make test` on a laptop without Docker stays usable, but it prints a banner and never calls the
// run a pass.
//
// Usage: go run ./hack/testverdict -reports DIR [-name LABEL] PKGDIR...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/onsi/ginkgo/v2/types"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

type verdict string

const (
	pass    verdict = "PASS"
	fail    verdict = "FAIL"
	skipped verdict = "SKIPPED"
	notRun  verdict = "NOT RUN"
)

type result struct {
	dir      string
	verdict  verdict
	passed   int
	requires []string // missing requirements that skipped specs (SKIPPED, or part of a PASS)
	why      string
}

func main() {
	reports := flag.String("reports", "", "directory with the Ginkgo JSON reports (*.json)")
	name := flag.String("name", "go-test", "label for the summary line")
	flag.Parse()
	if *reports == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: testverdict -reports DIR [-name LABEL] PKGDIR...")
		os.Exit(2)
	}
	byPath, err := load(*reports)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testverdict:", err)
		os.Exit(2)
	}
	required := os.Getenv(testenv.RequireVar)
	var results []result
	for _, dir := range flag.Args() {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "testverdict:", err)
			os.Exit(2)
		}
		results = append(results, judge(dir, byPath[abs], required))
	}
	if !summarize(os.Stdout, *name, results) {
		os.Exit(1)
	}
}

// load reads every *.json in dir as a Ginkgo report and indexes the suites by their directory.
func load(dir string) (map[string][]types.Report, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string][]types.Report{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var rs []types.Report
		if err := json.Unmarshal(b, &rs); err != nil {
			return nil, fmt.Errorf("%s: not a Ginkgo JSON report: %w", f, err)
		}
		for _, r := range rs {
			p := filepath.Clean(r.SuitePath)
			out[p] = append(out[p], r)
		}
	}
	return out, nil
}

// judge gives one suite its verdict from its reports (none when it never reported).
func judge(dir string, reports []types.Report, required string) result {
	res := result{dir: dir}
	if len(reports) == 0 {
		res.verdict, res.why = notRun, "no report (the suite never reached RunSpecs, or its processes exited before reporting)"
		return res
	}
	reqs := map[string]bool{}
	undeclared := ""
	failed := ""
	for _, r := range reports {
		for _, s := range r.SpecReports {
			switch {
			case s.State.Is(types.SpecStateFailureStates):
				if failed == "" {
					failed = fmt.Sprintf("%s %q: %s", s.LeafNodeType, s.LeafNodeText, s.State)
				}
			case s.State == types.SpecStatePassed && s.LeafNodeType == types.NodeTypeIt:
				res.passed++
			case s.State == types.SpecStateSkipped && s.Failure.Message != "" && !propagated(s.Failure.Message):
				if n, ok := requirement(s.Failure.Message); ok {
					reqs[n] = true
				} else if undeclared == "" {
					undeclared = s.Failure.Message
				}
			}
		}
		if !r.SuiteSucceeded && failed == "" {
			failed = "the suite did not succeed: " + strings.Join(r.SpecialSuiteFailureReasons, "; ")
		}
	}
	for n := range reqs {
		res.requires = append(res.requires, n)
	}
	sort.Strings(res.requires)
	switch {
	case failed != "":
		res.verdict, res.why = fail, failed
	case res.passed > 0:
		res.verdict = pass
	case len(reqs) > 0 && undeclared == "":
		res.verdict = skipped
		for _, n := range res.requires {
			if testenv.Required(required, n) {
				res.verdict, res.why = fail, fmt.Sprintf("skipped for %s, which %s=%s requires", n, testenv.RequireVar, required)
			}
		}
	case undeclared != "":
		res.verdict, res.why = notRun, "0 specs ran; skipped without a declared requirement: "+undeclared
	default:
		res.verdict, res.why = notRun, "0 specs ran"
	}
	return res
}

// propagated reports whether Ginkgo wrote the message for a spec it skipped because another node
// skipped or failed ("Spec skipped because Skip() was called in BeforeAll"); that node's own
// report carries the reason, so these say nothing about it.
func propagated(msg string) bool { return strings.HasPrefix(msg, "Spec skipped because ") }

// requirement returns the name in a testenv skip message ("koinon:requires=docker: ...").
func requirement(msg string) (string, bool) {
	rest, ok := strings.CutPrefix(msg, testenv.Prefix)
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, ":")
	return name, name != ""
}

// summarize prints the verdicts and reports whether the run may count as green.
func summarize(w io.Writer, name string, results []result) bool {
	count := map[verdict]int{}
	var skippedDirs []string
	missing := map[string]int{}
	for _, r := range results {
		count[r.verdict]++
		switch r.verdict {
		case fail, notRun:
			fmt.Fprintf(w, "%-8s %s: %s\n", r.verdict, r.dir, r.why)
		case skipped:
			skippedDirs = append(skippedDirs, fmt.Sprintf("%s (%s)", r.dir, strings.Join(r.requires, ", ")))
			for _, n := range r.requires {
				missing[n]++
			}
		}
	}
	if len(skippedDirs) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		slices.Sort(names)
		fmt.Fprintf(w, "\n==== %s: %d suite(s) SKIPPED, NOT PASSED: missing %s ====\n", name, len(skippedDirs), strings.Join(names, ", "))
		for _, d := range skippedDirs {
			fmt.Fprintf(w, "  SKIPPED  %s\n", d)
		}
		fmt.Fprintf(w, "Provide the requirement to run them; %s=%s turns these skips into failures (CI sets it).\n\n", testenv.RequireVar, strings.Join(names, ","))
	}
	ok := count[fail] == 0 && count[notRun] == 0
	state := "ok"
	switch {
	case !ok:
		state = "FAILED"
	case count[skipped] > 0:
		state = "INCOMPLETE (skipped suites did not run)"
	}
	fmt.Fprintf(w, "%s verdict: %s: %d passed, %d failed, %d not run, %d skipped (of %d suites)\n",
		name, state, count[pass], count[fail], count[notRun], count[skipped], len(results))
	return ok
}
