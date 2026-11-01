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
//	PARTIAL  specs passed, but others skipped for a missing requirement (printed "PASS (partly skipped: docker)")
//	SKIPPED  no spec ran because a requirement is missing (testenv: "koinon:requires=<name>: ...")
//	NOT RUN  no report, or no spec ran without a declared requirement
//
// -expect names the machine prerequisites this run is meant to have (testenv.Required syntax, default
// "all"). A SKIPPED or PARTIAL suite is incomplete when a requirement it lacks is expected; the
// others are opt-in targets (e2e, openshell, ...) whose skips are normal here and only listed.
//
// Exit codes: 1 when any suite is FAIL or NOT RUN; else 3 when any suite is incomplete (an
// expected requirement was missing, so the run did not prove everything); else 0. A requirement
// listed in KOINON_REQUIRE turns a SKIPPED or PARTIAL suite into FAIL.
//
// Usage: go run ./hack/testverdict -reports DIR [-name LABEL] [-expect LIST] PKGDIR...
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
	partial verdict = "PARTIAL"
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
	// incomplete: SKIPPED or PARTIAL because of a requirement the run expected to have (-expect).
	incomplete bool
	expected   []string // the subset of requires that -expect names
}

func main() {
	reports := flag.String("reports", "", "directory with the Ginkgo JSON reports (*.json)")
	name := flag.String("name", "go-test", "label for the summary line")
	expect := flag.String("expect", "all", "requirements this run is meant to have (comma list or \"all\"); skips for others are opt-in")
	flag.Parse()
	if *reports == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: testverdict -reports DIR [-name LABEL] [-expect LIST] PKGDIR...")
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
		results = append(results, judge(dir, byPath[abs], required, *expect))
	}
	os.Exit(summarize(os.Stdout, *name, results))
}

// load reads every *.json in dir as a Ginkgo report and indexes the suites by their directory.
func load(dir string) (map[string][]types.Report, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string][]types.Report{}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- the report directory the calling script names
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
func judge(dir string, reports []types.Report, required, expect string) result {
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
	case res.passed > 0 && len(res.requires) == 0:
		res.verdict = pass
	case res.passed > 0:
		res.verdict = partial
		res.why = "PASS (partly skipped: " + strings.Join(res.requires, ", ") + ")"
	case len(reqs) > 0 && undeclared == "":
		res.verdict = skipped
	case undeclared != "":
		res.verdict, res.why = notRun, "0 specs ran; skipped without a declared requirement: "+undeclared
	default:
		res.verdict, res.why = notRun, "0 specs ran"
	}
	if res.verdict == skipped || res.verdict == partial {
		for _, n := range res.requires {
			if testenv.Required(required, n) {
				res.verdict, res.why = fail, fmt.Sprintf("skipped for %s, which %s=%s requires", n, testenv.RequireVar, required)
				return res
			}
		}
		for _, n := range res.requires {
			if testenv.Required(expect, n) {
				res.incomplete = true
				res.expected = append(res.expected, n)
			}
		}
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

// summarize prints the verdicts and returns the exit code: 1 when a suite is FAIL or NOT RUN, 3 when
// a suite is incomplete (skipped for an expected requirement), otherwise 0.
func summarize(w io.Writer, name string, results []result) int {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) } // stdout; nothing to do on error
	count := map[verdict]int{}
	var incomplete, optIn []string
	missing := map[string]bool{}
	for _, r := range results {
		count[r.verdict]++
		switch r.verdict {
		case fail, notRun:
			p("%-8s %s: %s\n", r.verdict, r.dir, r.why)
		case skipped, partial:
			label := "SKIPPED"
			if r.verdict == partial {
				label = "PARTLY"
			}
			entry := fmt.Sprintf("%s (%s)", r.dir, strings.Join(r.requires, ", "))
			if !r.incomplete {
				optIn = append(optIn, entry)
				continue
			}
			incomplete = append(incomplete, fmt.Sprintf("  %-8s %s\n", label, entry))
			for _, n := range r.expected {
				missing[n] = true
			}
		}
	}
	var names []string
	for n := range missing {
		names = append(names, n)
	}
	slices.Sort(names)
	if len(optIn) > 0 {
		p("opt-in suites not run here: %s\n", strings.Join(optIn, ", "))
	}
	if len(incomplete) > 0 {
		p("\n==== %s: %d suite(s) SKIPPED, NOT PASSED: missing %s ====\n", name, len(incomplete), strings.Join(names, ", "))
		for _, l := range incomplete {
			p("%s", l)
		}
		p("Provide the requirement to run them; %s=%s turns these skips into failures (CI sets it).\n\n", testenv.RequireVar, strings.Join(names, ","))
	}
	code := 0
	state := "ok"
	switch {
	case count[fail] > 0 || count[notRun] > 0:
		code, state = 1, "FAILED"
	case len(incomplete) > 0:
		code, state = 3, "INCOMPLETE (missing "+strings.Join(names, ",")+")"
	}
	p("%s verdict: %s: %d passed, %d partly skipped, %d failed, %d not run, %d skipped (of %d suites)\n",
		name, state, count[pass], count[partial], count[fail], count[notRun], count[skipped], len(results))
	return code
}
