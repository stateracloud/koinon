// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/project"
)

func TestTestverdict(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testverdict")
}

func spec(kind types.NodeType, state types.SpecState, msg string) types.SpecReport {
	return types.SpecReport{LeafNodeType: kind, LeafNodeText: "x", State: state, Failure: types.Failure{Message: msg}}
}

func report(ok bool, specs ...types.SpecReport) []types.Report {
	return []types.Report{{SuitePath: "/r/pkg", SuiteSucceeded: ok, SpecReports: specs}}
}

const docker = "koinon:requires=docker: docker is not available: no socket"

var _ = Describe("judge", func() {
	It("is NOT RUN without a report (the ginkgo -p timeout of old issue 507)", func() {
		Expect(judge("pkg", nil, "", "all").verdict).To(Equal(notRun))
	})

	It("is PASS (partly skipped) when a spec passed and another skipped for docker", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "", "all")
		Expect(r.verdict).To(Equal(partial))
		Expect(r.why).To(Equal("PASS (partly skipped: docker)"))
		Expect(r.incomplete).To(BeTrue())
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("is PASS when a spec passed and nothing skipped", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeIt, types.SpecStatePassed, "")), "", "all")
		Expect(r.verdict).To(Equal(pass))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is partial but not incomplete when only an opt-in requirement skipped", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "koinon:requires=openshell: not set"),
		), "", "docker,git,helm")
		Expect(r.verdict).To(Equal(partial))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is FAIL when a partly skipped suite lacks a requirement KOINON_REQUIRE lists", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "docker", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is SKIPPED but not incomplete when only an opt-in requirement is missing", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "koinon:requires=e2e: KOINON_E2E is not set")), "", "docker,git,helm")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is FAIL when a spec failed, whatever passed", func() {
		r := judge("pkg", report(false,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateFailed, "boom"),
		), "", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is SKIPPED when a BeforeSuite skipped for a declared requirement", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "", "all")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("ignores the skips Ginkgo propagates from a skipped BeforeAll (test/escape)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeAll, types.SpecStateSkipped, "koinon:requires=envtest: KUBEBUILDER_ASSETS is not set"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "Spec skipped because Skip() was called in BeforeAll"),
		), "", "all")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"envtest"}))
	})

	It("is FAIL when KOINON_REQUIRE lists the missing requirement", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker)), "envtest,docker", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is NOT RUN when everything skipped without a declared requirement (chart-smoke's 0 of 56)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "Docker is not available"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "", "all")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when one undeclared skip sits beside a declared one", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "not wired yet"),
		), "", "all")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when the suite ran no spec at all", func() {
		Expect(judge("pkg", report(true), "", "all").verdict).To(Equal(notRun))
	})

	It("counts the specs of every parallel process", func() {
		rs := append(report(true, spec(types.NodeTypeIt, types.SpecStatePassed, "")),
			report(true, spec(types.NodeTypeIt, types.SpecStatePassed, ""))...)
		Expect(judge("pkg", rs, "", "all").passed).To(Equal(2))
	})
})

var _ = Describe("summarize", func() {
	It("never calls a run with a skipped suite a pass, and exits 3", func() {
		var out bytes.Buffer
		code := summarize(&out, "go-test", []result{
			{dir: "a", verdict: pass, passed: 3},
			{dir: "b", verdict: skipped, requires: []string{"docker"}, incomplete: true, expected: []string{"docker"}},
		})
		Expect(code).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("SKIPPED, NOT PASSED: missing docker"))
		Expect(out.String()).To(ContainSubstring("  SKIPPED  b (docker)"))
		Expect(out.String()).To(ContainSubstring("INCOMPLETE (missing docker)"))
		Expect(out.String()).NotTo(ContainSubstring("PASS "))
	})

	It("exits 3 for a partly skipped suite with an expected requirement", func() {
		var out bytes.Buffer
		r := judge("a", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "", "docker,git")
		Expect(r.why).To(Equal("PASS (partly skipped: docker)"))
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("  PARTLY   a (docker)"))
		Expect(out.String()).To(ContainSubstring("INCOMPLETE (missing docker)"))
		Expect(out.String()).To(ContainSubstring("1 partly skipped"))
	})

	It("lists opt-in partial and skipped suites and exits 0", func() {
		var out bytes.Buffer
		results := []result{
			{dir: "a", verdict: pass, passed: 3},
			{dir: "b", verdict: partial, passed: 1, requires: []string{"openshell"}},
			{dir: "c", verdict: skipped, requires: []string{"e2e"}},
		}
		Expect(summarize(&out, "go-test", results)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("opt-in suites not run here: c (e2e)"))
		Expect(out.String()).To(ContainSubstring("partly run here (the specs that need them skipped): b (openshell)"))
		Expect(out.String()).NotTo(ContainSubstring("NOT PASSED"))
		Expect(out.String()).To(ContainSubstring("go-test verdict: ok:"))
	})

	It("names only the expected requirements in the banner", func() {
		var out bytes.Buffer
		r := judge("a", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "koinon:requires=git: git is not on PATH")), "", "docker,git,helm")
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("(missing docker,git)"))
	})

	It("exits 3 for a SKIPPED suite lacking an expected docker", func() {
		var out bytes.Buffer
		r := judge("a", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker)), "", "docker,git,helm")
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
	})

	It("fails a run with a NOT RUN suite", func() {
		var out bytes.Buffer
		Expect(summarize(&out, "go-test", []result{{dir: "a", verdict: notRun, why: "no report"}})).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("NOT RUN  a: no report"))
	})

	It("lets FAIL beat incomplete", func() {
		var out bytes.Buffer
		code := summarize(&out, "go-test", []result{
			{dir: "a", verdict: fail, why: "boom"},
			{dir: "b", verdict: skipped, requires: []string{"docker"}, incomplete: true, expected: []string{"docker"}},
		})
		Expect(code).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("FAILED"))
	})
})

var _ = Describe("summarize, opt-in", func() {
	It("says partly run for a suite that ran but skipped specs for an opt-in requirement", func() {
		var out bytes.Buffer
		r := judge("a", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "koinon:requires=openshell: not set"),
		), "", "docker,git,helm")
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("opt-in suites partly run here (the specs that need them skipped): a (openshell)"))
		Expect(out.String()).NotTo(ContainSubstring("not run here"))
	})
})

// inModuleRoot runs the current spec from the module root, where loadGoTest's package directories
// ("hack/testverdict/testdata/x") resolve, as they do when hack/go-test.sh runs the binary.
func inModuleRoot() {
	BeforeEach(func() {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Chdir("../..")).To(Succeed())
		DeferCleanup(func() { Expect(os.Chdir(wd)).To(Succeed()) })
	})
}

var _ = Describe("go test events (testenv.RequireT in a plain test)", func() {
	write := func(lines ...string) string {
		f := filepath.Join(GinkgoT().TempDir(), "e.gotest.jsonl")
		Expect(os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o600)).To(Succeed())
		return f
	}
	// pkg is a fixture with a TestChartsmoke that calls RunSpecs and a plain TestChartContract, so the AST
	// lookup (relative to the module root) is exercised. gone has no directory: only the output fallback
	// can tell its suite test apart.
	const (
		pkg    = project.ModulePath + "/hack/testverdict/testdata/plainsuite"
		gone   = project.ModulePath + "/internal/no-such-package"
		fixDir = "hack/testverdict/testdata/plainsuite"
	)
	inModuleRoot()

	It("counts a plain test skipped for helm into its suite, which is then incomplete", func() {
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"run","Package":"`+pkg+`","Test":"TestChartContract"}`,
			`{"Action":"output","Package":"`+pkg+`","Test":"TestChartContract","Output":"    contract_test.go:46: koinon:requires=helm: helm is not on PATH\n"}`,
			`{"Action":"skip","Package":"`+pkg+`","Test":"TestChartContract"}`,
			`{"Action":"output","Package":"`+pkg+`","Test":"TestChartsmoke","Output":"Running Suite: chartsmoke\n"}`,
			`{"Action":"pass","Package":"`+pkg+`","Test":"TestChartsmoke"}`,
			`{"Action":"pass","Package":"`+pkg+`"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs[fixDir].requires()).To(Equal([]string{"helm"}))

		// The suite itself skipped for the opt-in envtest; the plain test's helm makes it incomplete.
		r := judgeWith("./test/chartsmoke", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "koinon:requires=envtest: KUBEBUILDER_ASSETS is not set"),
		), pkgs[fixDir].requires(), pkgs[fixDir].passed, "", "docker,git,helm")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.incomplete).To(BeTrue())
		Expect(r.expected).To(Equal([]string{"helm"}))
	})

	It("reads partly run, not not run, when a plain test passed and the suite skipped (chartsmoke with helm, no envtest)", func() {
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"pass","Package":"`+pkg+`","Test":"TestChartContract"}`,
			`{"Action":"pass","Package":"`+pkg+`","Test":"TestChartsmoke"}`,
		), pkgs)).To(Succeed())
		// The RunSpecs test passes whatever the specs did; only TestChartContract counts.
		Expect(pkgs[fixDir].passed).To(Equal(1))

		r := judgeWith("./test/chartsmoke", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "koinon:requires=envtest: KUBEBUILDER_ASSETS is not set"),
		), nil, pkgs[fixDir].passed, "", "docker,git,helm")
		Expect(r.verdict).To(Equal(partial))
		Expect(r.incomplete).To(BeFalse())
		var out bytes.Buffer
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("opt-in suites partly run here"))
		Expect(out.String()).NotTo(ContainSubstring("not run here"))
	})

	It("keeps NOT RUN when specs skipped without a declared requirement, whatever plain tests passed", func() {
		r := judgeWith("./internal/x", report(true,
			spec(types.NodeTypeIt, types.SpecStateSkipped, "no docker here"),
		), nil, 5, "", "")
		Expect(r.verdict).To(Equal(notRun))
		r = judgeWith("./internal/x", report(true), nil, 5, "", "")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("does not count a suite's own RunSpecs test as a passed plain test (found by AST, no output line)", func() {
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"pass","Package":"`+pkg+`","Test":"TestChartsmoke"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs[fixDir].passed).To(Equal(0))
		r := judgeWith("./test/chartsmoke", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "koinon:requires=envtest: x"),
		), nil, pkgs[fixDir].passed, "", "docker,git,helm")
		Expect(r.verdict).To(Equal(skipped))
	})

	It("falls back to the Running Suite output when the directory cannot be read", func() {
		// gone has no directory, so only the output line says TestSuite ran a Ginkgo suite.
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"output","Package":"`+gone+`","Test":"TestSuite","Output":"Running Suite: x\n"}`,
			`{"Action":"pass","Package":"`+gone+`","Test":"TestSuite"}`,
			`{"Action":"pass","Package":"`+gone+`","Test":"TestPlain"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs["internal/no-such-package"].passed).To(Equal(1))
	})

	It("keeps the output fallback when the AST does not name the test (a RunSpecs it cannot see)", func() {
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"output","Package":"`+pkg+`","Test":"TestElsewhere","Output":"Running Suite: x\n"}`,
			`{"Action":"pass","Package":"`+pkg+`","Test":"TestElsewhere"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs[fixDir].passed).To(Equal(0))
	})

	It("does not count a suite that calls RunSpecs through a helper, without the output line", func() {
		const helperPkg = project.ModulePath + "/hack/testverdict/testdata/helperpkg"
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"pass","Package":"`+helperPkg+`","Test":"TestSuiteViaHelper"}`,
			`{"Action":"pass","Package":"`+helperPkg+`","Test":"TestPlainOK"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs["hack/testverdict/testdata/helperpkg"].passed).To(Equal(1))
	})

	It("ignores a skip that names no requirement", func() {
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(write(
			`{"Action":"output","Package":"`+pkg+`","Test":"TestX","Output":"    x_test.go:1: short mode\n"}`,
			`{"Action":"skip","Package":"`+pkg+`","Test":"TestX"}`,
		), pkgs)).To(Succeed())
		Expect(pkgs[fixDir].requires()).To(BeEmpty())
	})

	It("rejects a file that is not a go test event stream", func() {
		Expect(loadGoTest(write("not json"), map[string]*plainPkg{})).NotTo(Succeed())
	})
})

var _ = Describe("loadGoTest", func() {
	// Two gotestsum phases, as hack/go-test.sh writes them: the first fails, the second passes.
	It("remembers a failing test of an earlier event file after a later file passed (old issue 512)", func() {
		dir := GinkgoT().TempDir()
		first := filepath.Join(dir, "plain.gotest.jsonl")
		second := filepath.Join(dir, "ginkgo-rest.gotest.jsonl")
		pkg := project.ModulePath + "/internal/x"
		Expect(os.WriteFile(first, []byte(`{"Action":"fail","Package":"`+pkg+`","Test":"TestBad"}
{"Action":"fail","Package":"`+pkg+`"}
`), 0o600)).To(Succeed())
		Expect(os.WriteFile(second, []byte(`{"Action":"pass","Package":"`+project.ModulePath+`/internal/y","Test":"TestGood"}
`), 0o600)).To(Succeed())
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(first, pkgs)).To(Succeed())
		Expect(loadGoTest(second, pkgs)).To(Succeed())
		Expect(pkgs["internal/x"].failed).To(Equal([]string{"TestBad", "(package)"}))
		Expect(pkgs["internal/y"].failed).To(BeEmpty())
		Expect(pkgs["internal/y"].passed).To(Equal(1))
	})
})

var _ = Describe("plainRequireTests (parallel suites emit no go test events)", func() {
	dirWith := func(src string) string {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600)).To(Succeed())
		return dir
	}

	It("finds a plain test that calls RequireT", func() {
		dir := dirWith(`package x
import "testing"
func TestSuite(t *testing.T) { RunSpecs(t, "x") }
func TestNeedsHelm(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
func TestPlain(t *testing.T) {}
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestNeedsHelm"}))
	})

	It("ignores the RunSpecs test and Ginkgo specs", func() {
		dir := dirWith(`package x
import "testing"
func TestSuite(t *testing.T) { testenv.RequireT(t, testenv.Docker); RunSpecs(t, "x") }
`)
		Expect(plainRequireTests(dir)).To(BeEmpty())
	})

	It("names the offending tests in the failure", func() {
		Expect(parallelPlainWhy([]string{"TestA"})).To(And(ContainSubstring("ginkgo -p"), ContainSubstring("TestA")))
	})

	It("finds a plain test that reaches RequireT through a package-local helper", func() {
		dir := dirWith(`package x
import "testing"
func needHelm(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
func TestViaHelper(t *testing.T) { needHelm(t) }
func TestClean(t *testing.T) { other(t) }
func other(t *testing.T) {}
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestViaHelper"}))
	})

	It("follows a chain of package-local helpers", func() {
		dir := dirWith(`package x
import "testing"
func deep(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
func mid(t *testing.T) { deep(t) }
func TestTwoLevels(t *testing.T) { mid(t) }
func TestClean(t *testing.T) {}
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestTwoLevels"}))
	})

	It("finds RequireT through a helper in a non-test file", func() {
		Expect(plainRequireTests("testdata/helperpkg")).To(Equal([]string{"TestHelmViaFile"}))
	})

	It("ignores a test that reaches RunSpecs through a helper", func() {
		dir := dirWith(`package x
import "testing"
func run(t *testing.T) { RunSpecs(t, "x"); testenv.RequireT(t, testenv.Docker) }
func TestSuite(t *testing.T) { run(t) }
`)
		Expect(plainRequireTests(dir)).To(BeEmpty())
	})

	It("terminates on recursive helpers and still finds RequireT behind them", func() {
		dir := dirWith(`package x
import "testing"
func ping(t *testing.T) { pong(t); ping(t) }
func pong(t *testing.T) { ping(t); testenv.RequireT(t, testenv.Helm) }
func loopA(t *testing.T) { loopB(t) }
func loopB(t *testing.T) { loopA(t) }
func TestHit(t *testing.T) { ping(t) }
func TestLoopOnly(t *testing.T) { loopA(t) }
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestHit"}))
	})

	It("does not follow a helper in another package", func() {
		dir := dirWith(`package x
import "testing"
func TestOther(t *testing.T) { otherpkg.Helper(t) }
`)
		Expect(plainRequireTests(dir)).To(BeEmpty())
	})

	It("finds RequireT through a method called on a receiver", func() {
		dir := dirWith(`package x
import "testing"
type env struct{}
func (e *env) need(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
func TestViaMethod(t *testing.T) { e := &env{}; e.need(t) }
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestViaMethod"}))
	})

	It("does not take a method for a test, and does not follow a call qualified by an imported package", func() {
		dir := dirWith(`package x
import (
	"testing"
	"example.com/other"
)
type env struct{}
func (e *env) TestNot(t *testing.T) {}
func Run(t *testing.T) { RunSpecs(t, "x") }
func TestQualified(t *testing.T) { testenv.RequireT(t, testenv.Helm); other.Run(t) }
`)
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestQualified"}))
		Expect(runSpecsTests(dir)).To(BeEmpty())
	})

	It("unions the bodies of a name declared twice (build-tagged variants) instead of letting the last win", func() {
		dir := dirWith(`package x
import "testing"
func needHelm(t *testing.T) { testenv.RequireT(t, testenv.Helm) }
func TestV(t *testing.T) { needHelm(t) }
`)
		Expect(os.WriteFile(filepath.Join(dir, "y_test.go"), []byte(`package x
import "testing"
func needHelm(t *testing.T) {}
`), 0o600)).To(Succeed())
		Expect(plainRequireTests(dir)).To(Equal([]string{"TestV"}))
	})

	It("has none in the real parallel suites", func() {
		out, err := exec.Command("bash", "../go-test.sh", "--list").Output()
		Expect(err).NotTo(HaveOccurred())
		var suites []string
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				suites = append(suites, line)
			}
		}
		Expect(suites).NotTo(BeEmpty())
		for _, d := range suites {
			Expect(strings.HasPrefix(d, "./internal/")).To(BeTrue(), d)
			Expect(plainRequireTests("../../"+d)).To(BeEmpty(), d)
		}
	})
})

var _ = Describe("runSpecsTests", func() {
	It("finds the Test functions that call RunSpecs", func() {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(`package x
import "testing"
func TestSuite(t *testing.T) { ginkgo.RunSpecs(t, "x") }
func TestPlain(t *testing.T) {}
`), 0o600)).To(Succeed())
		Expect(runSpecsTests(dir)).To(Equal(map[string]bool{"TestSuite": true}))
	})

	It("errors on a directory with no test files", func() {
		_, err := runSpecsTests(GinkgoT().TempDir())
		Expect(err).To(HaveOccurred())
	})

	It("does not count the RunSpecs test as a passed plain test even without the Running Suite line (-ginkgo.succinct)", func() {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(`package x
import "testing"
func TestSuite(t *testing.T) { RunSpecs(t, "x") }
func TestPlain(t *testing.T) {}
`), 0o600)).To(Succeed())
		// loadGoTest keys packages by directory relative to the module root, so run from there.
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		rel, err := filepath.Rel(wd, dir)
		Expect(err).NotTo(HaveOccurred())
		pkg := project.ModulePath + "/" + filepath.ToSlash(rel)
		f := filepath.Join(GinkgoT().TempDir(), "e.jsonl")
		Expect(os.WriteFile(f, []byte(`{"Action":"pass","Package":"`+pkg+`","Test":"TestSuite"}
{"Action":"pass","Package":"`+pkg+`","Test":"TestPlain"}
`), 0o600)).To(Succeed())
		pkgs := map[string]*plainPkg{}
		Expect(loadGoTest(f, pkgs)).To(Succeed())
		Expect(pkgs[filepath.ToSlash(rel)].passed).To(Equal(1))
	})
})

var _ = Describe("parallel plain-test verdict", func() {
	It("keeps an earlier failure reason when a plain RequireT test is found too", func() {
		res := result{dir: "./internal/x", verdict: fail, passed: 2, why: "spec failed", requires: []string{"docker"}}
		got := mergeParallelPlain(res, []string{"TestA"})
		Expect(got.verdict).To(Equal(fail))
		Expect(got.why).To(And(ContainSubstring("spec failed"), ContainSubstring("TestA")))
		Expect(got.requires).To(Equal([]string{"docker"}))
		Expect(got.passed).To(Equal(2))
	})

	It("turns a passing suite into a failure naming the tests", func() {
		got := mergeParallelPlain(result{dir: "./internal/x", verdict: pass, passed: 3}, []string{"TestA"})
		Expect(got.verdict).To(Equal(fail))
		Expect(got.why).To(ContainSubstring("TestA"))
		Expect(got.passed).To(Equal(3))
	})
})
