// SPDX-License-Identifier: Apache-2.0

package compat_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/compat"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// checkFolder runs the three checks on one release folder. The Postgres one skips without Docker.
func checkFolder(dir string) {
	GinkgoHelper()
	It("loads the stored agent configs", func() {
		Expect(compat.CheckAgentConfigs(dir)).To(Succeed())
	})
	It("restores the recorded home backup", func(ctx SpecContext) {
		Expect(compat.CheckHomeBackup(ctx, dir)).To(Succeed())
	}, SpecTimeout(2*time.Minute))
	It("reads the home checkpoints after migrating from the recorded schema to head", func(ctx SpecContext) {
		Expect(compat.CheckHomeCheckpoints(ctx, pgtest.Run(ctx), dir)).To(Succeed())
	}, SpecTimeout(3*time.Minute))
}

// The compatibility check itself: every release folder recorded under testdata/compat/.
var _ = Describe("recorded releases", func() {
	root := filepath.Join(repoRoot(), compat.Root)
	releases, err := compat.Releases(root)
	if err != nil {
		It("lists the recorded releases", func() { Expect(err).NotTo(HaveOccurred()) })
	}
	if len(releases) == 0 {
		It("has no recorded release yet", func() {
			AddReportEntry("NO RECORDED RELEASE under "+compat.Root+": nothing was checked. The first folder is recorded at the v0.1.0 cut (docs/dev/releasing.md); until then this spec proves nothing about compatibility.", ReportEntryVisibilityAlways)
		})
	}
	for _, r := range releases {
		Describe(r, func() { checkFolder(filepath.Join(root, r)) })
	}
})

// The harness proof, NOT the compatibility check: a folder recorded by hack/record-compat-fixtures.sh
// from the code at head into a temp dir, then loaded by the same code. It shows the recorder and
// the loader agree (and that a loader that cannot fail would be caught below); it says nothing about
// older releases.
var _ = Describe("harness self-test (a folder recorded at head, not a compatibility check)", Ordered, func() {
	var dir string
	BeforeAll(func(ctx SpecContext) {
		dir = filepath.Join(GinkgoT().TempDir(), "selftest")
		cmd := exec.CommandContext(ctx, filepath.Join(repoRoot(), "hack", "record-compat-fixtures.sh"), "selftest", "--out", dir)
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		m, err := compat.ReadManifest(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Release).To(Equal("selftest"))
	}, NodeTimeout(5*time.Minute))

	It("loads the stored agent configs", func() {
		Expect(compat.CheckAgentConfigs(dir)).To(Succeed())
	})
	It("restores the recorded home backup", func(ctx SpecContext) {
		Expect(compat.CheckHomeBackup(ctx, dir)).To(Succeed())
	}, SpecTimeout(2*time.Minute))
	It("reads the home checkpoints after migrating from the recorded schema to head", func(ctx SpecContext) {
		Expect(compat.CheckHomeCheckpoints(ctx, pgtest.Run(ctx), dir)).To(Succeed())
	}, SpecTimeout(3*time.Minute))

	It("fails when a stored config decodes differently than recorded", func() {
		entries, err := filepath.Glob(filepath.Join(dir, "agent_configs", "*.decoded.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).NotTo(BeEmpty())
		Expect(os.WriteFile(entries[0], []byte(`{"instructions":"something the reader does not produce"}`), 0o644)).To(Succeed())
		DeferCleanup(func() { _ = os.RemoveAll(filepath.Join(dir, "agent_configs")) })
		Expect(compat.CheckAgentConfigs(dir)).NotTo(Succeed())
	})
	It("refuses to overwrite a recorded folder", func(ctx SpecContext) {
		Expect(compat.Record(ctx, dir, "again")).NotTo(Succeed())
	})
})
