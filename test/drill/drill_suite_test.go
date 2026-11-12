// SPDX-License-Identifier: Apache-2.0

// The restore drill (ADR 0109, issue #514): a Ginkgo suite that seeds a source Koinon install, backs
// it up with the guide's own commands, declares it lost, restores into a fresh namespace with its own
// OpenShell gateway, holds it, checks it, releases it and checks it again. It runs with
// test/drill/run.sh, which makes the cluster, loads the images and passes:
//
//	KOINON_DRILL_KUBECONFIG  the cluster's kubeconfig file (explicit; the ambient context is never used)
//	KOINON_DRILL_TOPOLOGY    the topology file (test/drill/topology/kind.yaml)
//	KOINON_DRILL_REPO        the repository root (the chart, the guide's command file, the fake model script)
//	KOINON_DRILL_OUT         the directory for drill-result.json and the work files
//	KOINON_DRILL_RUN         optional: the run ID (3 to 12 lowercase letters and digits); default random
//	KOINON_DRILL_KEEP        optional: 1 leaves the namespaces for a look; run sweep.sh afterwards
//	KOINON_DRILL_COMMIT      optional: the commit under test, for drill-result.json
//
// Without KOINON_DRILL_KUBECONFIG the suite fails when KOINON_REQUIRE names "drill" (CI does) and
// skips otherwise, so `go test ./...` stays offline. A check the product cannot pass yet fails with
// "known gap: #N" and is labelled known-gap; nothing is skipped.
package drill_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/test/drill"
)

func TestDrill(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Restore drill (ADR 0109)")
}

// env is what BeforeSuite resolved.
var (
	runID    string
	top      *drill.Topology
	kube     *drill.Kube
	repoRoot string
	outDir   string
	workDir  string
	res      drill.Result
)

var _ = BeforeSuite(func() {
	testenv.Require(
		testenv.Var("drill", "KOINON_DRILL_KUBECONFIG"),
		testenv.Var("drill", "KOINON_DRILL_TOPOLOGY"),
		testenv.Var("drill", "KOINON_DRILL_REPO"),
		testenv.Binary("kubectl"),
		testenv.Binary("helm"),
	)
	runID = os.Getenv("KOINON_DRILL_RUN")
	if runID == "" {
		runID = drill.NewRunID()
	}
	Expect(drill.CheckRunID(runID)).To(Succeed())
	repoRoot = os.Getenv("KOINON_DRILL_REPO")
	outDir = os.Getenv("KOINON_DRILL_OUT")
	if outDir == "" {
		outDir = GinkgoT().TempDir()
	}
	Expect(os.MkdirAll(outDir, 0o755)).To(Succeed())
	workDir = filepath.Join(outDir, "work")
	Expect(os.MkdirAll(workDir, 0o755)).To(Succeed())

	var err error
	top, err = drill.LoadTopology(os.Getenv("KOINON_DRILL_TOPOLOGY"), runID)
	Expect(err).NotTo(HaveOccurred())
	if top.BrowserSignin {
		testenv.Require(testenv.BinaryNamed("drill", "pnpm"))
	}
	kube, err = drill.NewKube(os.Getenv("KOINON_DRILL_KUBECONFIG"), top.Context, filepath.Join(outDir, "bin"))
	Expect(err).NotTo(HaveOccurred())

	res = drill.Result{RunID: runID, Commit: os.Getenv("KOINON_DRILL_COMMIT"), Topology: top, Started: time.Now().UTC()}
	AddReportEntry("run", runID)
	AddReportEntry("topology", top.Name)
})

// The result file is written from the spec reports, so it exists after a red run too.
var _ = ReportAfterSuite("drill-result.json", func(report Report) {
	if top == nil {
		return // BeforeSuite did not get far enough to have a drill
	}
	for _, sr := range report.SpecReports {
		if sr.LeafNodeType.String() != "It" {
			continue
		}
		c := drill.Check{Name: checkName(sr.Labels())}
		switch {
		case sr.State.Is(types.SpecStatePassed):
			c.Verdict = drill.Pass
		case sr.State.Is(types.SpecStateFailureStates):
			c.Message = sr.Failure.Message
			c.Verdict, c.Issue = drill.ClassifyFailure(sr.Labels(), sr.Failure.Message)
		default:
			c.Verdict = drill.NotRun
		}
		res.Checks = append(res.Checks, c)
	}
	res.Finished = time.Now().UTC()
	res.Chart = chartVersion()
	res.Finish()
	Expect(res.Write(filepath.Join(outDir, "drill-result.json"))).To(Succeed())
})

// checkName is the label that names the check: the last one that is not a category.
func checkName(labels []string) string {
	name := ""
	for _, l := range labels {
		if l != "step" && l != "known-gap" {
			name = l
		}
	}
	return name
}

// chartVersion reads the chart under test's version from its Chart.yaml.
func chartVersion() string {
	b, err := os.ReadFile(filepath.Join(top.Path(repoRoot, top.ChartPath), "Chart.yaml"))
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`(?m)^version:\s*(\S+)`).FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}
