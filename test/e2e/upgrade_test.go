// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
	"github.com/stateracloud/koinon/test/e2e/upgradecheck"
)

// The N-1 to N upgrade check (issue 519). test/e2e/upgrade.sh installs the previous release, seeds it with that
// release's own quickstart driver, upgrades the same release to this build with `helm upgrade`, and writes one
// snapshot of the database before and one after (the format of upgradecheck). This spec judges the two snapshots
// and the state of the Deployments. It is not part of the quickstart pass (the label filter leaves it out);
// upgrade.sh runs it with KOINON_E2E_UPGRADE_FROM set. Without that variable the spec skips.
var _ = Describe("the upgrade from the previous release", Ordered, Label("upgrade"), func() {
	var before, after upgradecheck.Snapshot

	BeforeAll(func() {
		// Declared here, not in BeforeSuite: the quickstart and stories passes must not need them.
		testenv.Require(
			testenv.Vars("e2e-upgrade", "KOINON_E2E_UPGRADE_FROM", "KOINON_E2E_UPGRADE_BEFORE", "KOINON_E2E_UPGRADE_AFTER", "KOINON_E2E_KUBECTL"),
			testenv.Binary("kubectl"),
		)
		read := func(variable string) upgradecheck.Snapshot {
			GinkgoHelper()
			raw, err := os.ReadFile(os.Getenv(variable))
			Expect(err).NotTo(HaveOccurred(), variable)
			s, err := upgradecheck.Parse(string(raw))
			Expect(err).NotTo(HaveOccurred(), variable)
			return s
		}
		before, after = read("KOINON_E2E_UPGRADE_BEFORE"), read("KOINON_E2E_UPGRADE_AFTER")
	})

	It("seeded the previous release with what an upgrade must keep", func() {
		// A comparison of zero with zero proves nothing: the quickstart pass leaves an agent, a task, a run and its sandbox.
		Expect(upgradecheck.Empty(before, "agents", "tasks", "runs", "sandboxes")).To(BeEmpty())
	})

	It("keeps the install, its keys, its agents, tasks, runs and sandboxes, and the sandboxes are back in their state", func() {
		Expect(upgradecheck.Compare(before, after)).To(BeEmpty())
	})

	It("migrated the schema at least to where the previous release left it", func() {
		Expect(after["migration"]).NotTo(BeEmpty())
	})

	It("converges: every Koinon Deployment is available on the new build", func() {
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		out, err := exec.Command(kubectl, "-n", "koinon", "get", "deploy", "-l", "app.kubernetes.io/instance=koinon",
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.availableReplicas}{"/"}{.spec.replicas}{"\n"}{end}`).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		lines := strings.Fields(string(out))
		Expect(lines).NotTo(BeEmpty(), "no Deployment of the release")
		for _, l := range lines {
			name, ratio, _ := strings.Cut(l, "=")
			have, want, _ := strings.Cut(ratio, "/")
			Expect(have).To(Equal(want), "%s is not fully available: %s", name, ratio)
		}
	})
})
