// SPDX-License-Identifier: Apache-2.0

// Command testverdict decides whether a run of Ginkgo suites really ran (old issue 507). Exit codes from
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
// A plain `func TestX` test that skips through testenv.RequireT never reaches a Ginkgo report;
// -gotest-json reads the `go test -json` events (gotestsum --jsonfile) and counts its requirement
// for its package like a spec's.
//
// A plain test that passes counts like a passing spec for its package, so a suite whose specs all
// skipped but whose plain tests ran reads "partly run", not "not run". The test that calls RunSpecs
// is not counted (it passes whatever the specs did). Suites run by `ginkgo -p` (-parallel) give no
// go test events at all; a plain test calling testenv.RequireT in one of them is a FAIL, found by
// parsing the package's test files.
//
// Usage: go run ./hack/testverdict -reports DIR [-name LABEL] [-expect LIST] [-gotest-json FILE]... [-parallel DIR]... PKGDIR...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/onsi/ginkgo/v2/types"

	"github.com/stateracloud/koinon/internal/project"
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
	var gotest stringList
	flag.Var(&gotest, "gotest-json", "a `go test -json` event file (repeatable): plain tests skipped through testenv.RequireT")
	var parallel stringList
	flag.Var(&parallel, "parallel", "a suite directory run by `ginkgo -p` (repeatable): it emits no go test events, so a plain RequireT test there is an error")
	flag.Parse()
	parallelDirs := map[string]bool{}
	for _, d := range parallel {
		parallelDirs[filepath.Clean(d)] = true
	}
	if *reports == "" || (flag.NArg() == 0 && len(gotest) == 0) {
		fmt.Fprintln(os.Stderr, "usage: testverdict -reports DIR [-name LABEL] [-expect LIST] [-gotest-json FILE]... [-parallel DIR]... PKGDIR...")
		os.Exit(2)
	}
	byPath, err := load(*reports)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testverdict:", err)
		os.Exit(2)
	}
	plain := map[string]*plainPkg{}
	for _, f := range gotest {
		if err := loadGoTest(f, plain); err != nil {
			fmt.Fprintln(os.Stderr, "testverdict:", err)
			os.Exit(2)
		}
	}
	required := os.Getenv(testenv.RequireVar)
	var results []result
	suites := map[string]bool{}
	for _, dir := range flag.Args() {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "testverdict:", err)
			os.Exit(2)
		}
		suites[filepath.Clean(dir)] = true
		var extra []string
		plainPassed := 0
		if p := plain[filepath.Clean(dir)]; p != nil {
			extra, plainPassed = p.requires(), p.passed
		}
		res := judgeWith(dir, byPath[abs], extra, plainPassed, required, *expect)
		if parallelDirs[filepath.Clean(dir)] {
			bad, err := plainRequireTests(dir)
			if err != nil {
				fmt.Fprintln(os.Stderr, "testverdict:", err)
				os.Exit(2)
			}
			if len(bad) > 0 {
				res = mergeParallelPlain(res, bad)
			}
		}
		results = append(results, res)
	}
	// A package without a Ginkgo suite gets a verdict only when one of its tests skipped through
	// testenv.RequireT; its other outcomes are go test's own exit code.
	dirs := make([]string, 0, len(plain))
	for d := range plain {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		p := plain[d]
		if len(p.failed) > 0 {
			// Whatever the package's Ginkgo report says: a failed plain test fails the run, and the
			// verdict must not read "0 failed" beside go test's own FAIL (old issue 512).
			results = append(results, result{dir: "./" + d, verdict: fail, passed: p.passed, why: "plain test failed: " + strings.Join(p.failed, ", ")})
			continue
		}
		if suites[d] || len(p.skips) == 0 {
			continue
		}
		res := result{dir: "./" + d, passed: p.passed, requires: p.requires(), verdict: skipped}
		if p.passed > 0 {
			res.verdict, res.why = partial, "PASS (partly skipped: "+strings.Join(res.requires, ", ")+")"
		}
		results = append(results, classify(res, required, *expect))
	}
	os.Exit(summarize(os.Stdout, *name, results))
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// plainPkg is what the go test events say about one package's plain (`func TestX`) tests.
type plainPkg struct {
	passed int             // top-level tests that passed
	skips  map[string]bool // requirements a test skipped for (testenv.RequireT)
	failed []string        // plain tests that failed (and the package itself, when it failed as a whole)
}

func (p *plainPkg) requires() []string {
	out := make([]string, 0, len(p.skips))
	for n := range p.skips {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// loadGoTest reads a `go test -json` event stream (gotestsum --jsonfile) into pkgs, keyed by the
// package directory relative to the module root ("internal/x", "." for the root).
func loadGoTest(file string, pkgs map[string]*plainPkg) error {
	f, err := os.Open(file) // #nosec G304 -- the event file the calling script names
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	type event struct{ Action, Package, Test, Output string }
	pending := map[[2]string]string{}         // (package, test) -> requirement named in its output
	runSpecs := map[[2]string]bool{}          // tests that ran a Ginkgo suite: their pass says nothing on their own
	specTests := map[string]map[string]bool{} // package dir -> Test funcs calling RunSpecs (AST); nil when unreadable
	dec := json.NewDecoder(f)
	for {
		var e event
		if err := dec.Decode(&e); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: not a go test -json stream: %w", file, err)
		}
		if e.Test == "" && e.Action != "fail" {
			continue
		}
		dir := pkgDir(e.Package)
		p := pkgs[dir]
		if p == nil {
			p = &plainPkg{skips: map[string]bool{}}
			pkgs[dir] = p
		}
		if e.Test == "" { // the package's own result: build error, panic, timeout
			p.failed = append(p.failed, "(package)")
			continue
		}
		key := [2]string{e.Package, e.Test}
		switch e.Action {
		case "output":
			// The fallback for a RunSpecs the AST walk cannot see (an unreadable directory, a call
			// through another package).
			if strings.Contains(e.Output, "Running Suite:") {
				runSpecs[key] = true
			}
			if i := strings.Index(e.Output, testenv.Prefix); i >= 0 {
				if n, ok := requirement(strings.TrimSpace(e.Output[i:])); ok {
					pending[key] = n
				}
			}
		case "skip":
			if n, ok := pending[key]; ok {
				p.skips[n] = true
			}
		case "fail":
			if !strings.Contains(e.Test, "/") {
				p.failed = append(p.failed, e.Test)
			}
		case "pass":
			if strings.Contains(e.Test, "/") {
				break
			}
			st, seen := specTests[dir]
			if !seen {
				st, _ = runSpecsTests(dir) // nil on an unreadable directory
				specTests[dir] = st
			}
			// The AST names the suites it can see; the "Running Suite:" output covers the rest (a
			// RunSpecs the walk cannot reach, an unreadable directory). Either one marks a suite.
			isSuite := runSpecs[key] || st[e.Test]
			if !isSuite {
				p.passed++
			}
		}
	}
}

// mergeParallelPlain fails res for the plain RequireT tests in bad, keeping everything else it holds;
// an earlier failure reason is joined with the new one rather than replaced.
func mergeParallelPlain(res result, bad []string) result {
	why := parallelPlainWhy(bad)
	if res.verdict == fail && res.why != "" {
		why = res.why + "; " + why
	}
	res.verdict, res.why = fail, why
	return res
}

func parallelPlainWhy(tests []string) string {
	return "plain test(s) calling testenv.RequireT in a suite run by ginkgo -p, which emits no go test events, so their skips and passes are not counted: " +
		strings.Join(tests, ", ") + " (fix: make them Ginkgo specs, or take the package off PARALLEL_SUITES in hack/go-test.sh)"
}

// pkgFuncs is the package-level (receiver-less) functions of one package directory.
type pkgFuncs struct {
	tests  []string                    // names of the func TestX declarations of the *_test.go files, sorted
	bodies map[string][]*ast.BlockStmt // every receiver-less function with a body, by name (test and non-test files)
}

// parsePkgFuncs parses the .go files of dir: the *_test.go files give the tests, all of them (test and
// non-test) give the function bodies a helper walk can follow. A directory without test files is an
// error, so a caller can tell "no tests" from "could not look".
//
// parser.ParseFile ignores build tags, so the files of every platform and tag are read. bodies is keyed
// by name and keeps every body declared under that name (two files with opposite build tags can declare
// the same function), and a walk takes the union of their calls. A name clash can therefore only add
// calls: a RequireT that exists in one variant is still found (a false FAIL at worst), and a RunSpecs in
// one variant makes the test count as a suite (its pass is not counted as a plain pass, which can only
// lower the verdict), never the other way round.
func parsePkgFuncs(dir string) (*pkgFuncs, error) {
	tests, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("%s: no test files", dir)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	pf := &pkgFuncs{bodies: map[string][]*ast.BlockStmt{}}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		isTest := strings.HasSuffix(f, "_test.go")
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			pf.bodies[name] = append(pf.bodies[name], fn.Body)
			if isTest && strings.HasPrefix(name, "Test") && !seen[name] {
				seen[name] = true
				pf.tests = append(pf.tests, name)
			}
		}
	}
	sort.Strings(pf.tests)
	return pf, nil
}

// callees returns the names a function body calls (`F(...)` and `x.F(...)` both give "F").
func callees(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			switch fun := c.Fun.(type) {
			case *ast.SelectorExpr:
				out[fun.Sel.Name] = true
			case *ast.Ident:
				out[fun.Name] = true
			}
		}
		return true
	})
	return out
}

// reaches reports whether the function name calls target, directly or through any chain of
// package-local functions (receiver-less, in the test or non-test files of the directory). A visited
// set ends recursion. The walk does not leave the package: a helper in another package that calls
// target is not followed.
func (pf *pkgFuncs) reaches(name, target string) bool {
	visited := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if visited[n] {
			return false
		}
		visited[n] = true
		for _, body := range pf.bodies[n] {
			calls := callees(body)
			if calls[target] {
				return true
			}
			for c := range calls {
				if _, local := pf.bodies[c]; local && walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(name)
}

// runSpecsTests returns the Test functions of the package in dir that call RunSpecs, directly or through
// a package-local helper: the tests that run a Ginkgo suite, whose go test pass says nothing on its own.
// It errors when the directory cannot be read.
func runSpecsTests(dir string) (map[string]bool, error) {
	pf, err := parsePkgFuncs(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range pf.tests {
		if pf.reaches(name, "RunSpecs") {
			out[name] = true
		}
	}
	return out, nil
}

// plainRequireTests returns the plain tests (func TestX(t *testing.T) that do not run a Ginkgo suite)
// of the package in dir that call RequireT, directly or through any chain of package-local functions
// declared in its test or non-test files. A plain test that never reaches RequireT is not reported.
// The remaining limit: only the package's own receiver-less functions are followed, so a helper in
// another package that calls RequireT is missed (the call is not visible in this directory). A
// directory without test files gives none.
func plainRequireTests(dir string) ([]string, error) {
	pf, err := parsePkgFuncs(dir)
	if err != nil {
		if files, _ := filepath.Glob(filepath.Join(dir, "*_test.go")); len(files) == 0 {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, name := range pf.tests {
		if !pf.reaches(name, "RunSpecs") && pf.reaches(name, "RequireT") {
			out = append(out, name)
		}
	}
	return out, nil
}

// pkgDir maps an import path to its directory relative to the module root.
func pkgDir(pkg string) string {
	if pkg == project.ModulePath {
		return "."
	}
	return strings.TrimPrefix(pkg, project.ModulePath+"/")
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
	return judgeWith(dir, reports, nil, 0, required, expect)
}

// judgeWith is judge with the requirements the package's plain tests skipped for (testenv.RequireT,
// which a Ginkgo report never sees), counted like a spec's, and the number of plain tests that passed,
// counted like passing specs.
func judgeWith(dir string, reports []types.Report, plainReqs []string, plainPassed int, required, expect string) result {
	res := result{dir: dir}
	specPassed := 0
	if len(reports) == 0 {
		res.verdict, res.why = notRun, "no report (the suite never reached RunSpecs, or its processes exited before reporting)"
		return res
	}
	reqs := map[string]bool{}
	for _, n := range plainReqs {
		reqs[n] = true
	}
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
				specPassed++
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
	// Plain tests only display and promote a skip for a declared requirement; they never stand in for
	// specs that did not run (an undeclared skip or 0 specs stays NOT RUN).
	res.passed = specPassed + plainPassed
	switch {
	case failed != "":
		res.verdict, res.why = fail, failed
	case specPassed > 0 && len(res.requires) == 0:
		res.verdict = pass
	case specPassed > 0 || (plainPassed > 0 && len(reqs) > 0 && undeclared == ""):
		res.verdict = partial
		res.why = "PASS (partly skipped: " + strings.Join(res.requires, ", ") + ")"
	case len(reqs) > 0 && undeclared == "":
		res.verdict = skipped
	case undeclared != "":
		res.verdict, res.why = notRun, "0 specs ran; skipped without a declared requirement: "+undeclared
	default:
		res.verdict, res.why = notRun, "0 specs ran"
	}
	return classify(res, required, expect)
}

// classify settles a SKIPPED or PARTIAL result: FAIL when KOINON_REQUIRE lists a missing
// requirement, incomplete when -expect does, otherwise opt-in.
func classify(res result, required, expect string) result {
	if res.verdict != skipped && res.verdict != partial {
		return res
	}
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
	var incomplete, optIn, partlyOptIn []string
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
				if r.verdict == partial {
					partlyOptIn = append(partlyOptIn, entry)
				} else {
					optIn = append(optIn, entry)
				}
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
	if len(partlyOptIn) > 0 {
		p("opt-in suites partly run here (the specs that need them skipped): %s\n", strings.Join(partlyOptIn, ", "))
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
