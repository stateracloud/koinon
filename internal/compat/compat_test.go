// SPDX-License-Identifier: Apache-2.0

package compat_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
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
	It("reads the home checkpoints after migrating to head and restores the home backup with the password from its envelope", func(ctx SpecContext) {
		repo, err := compat.CheckHomeCheckpoints(ctx, pgtest.Run(ctx), dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(compat.CheckHomeBackup(ctx, dir, repo)).To(Succeed())
	}, SpecTimeout(5*time.Minute))
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
	It("reads the home checkpoints after migrating to head and restores the home backup with the password from its envelope", func(ctx SpecContext) {
		repo, err := compat.CheckHomeCheckpoints(ctx, pgtest.Run(ctx), dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(compat.CheckHomeBackup(ctx, dir, repo)).To(Succeed())
	}, SpecTimeout(5*time.Minute))

	It("records a full config that sets every field of agentconfig.Config", func() {
		for _, noTemplate := range []bool{false, true} {
			raw, err := compat.FullConfig(noTemplate).Marshal()
			Expect(err).NotTo(HaveOccurred())
			var got map[string]any
			Expect(json.Unmarshal(raw, &got)).To(Succeed())
			typ := reflect.TypeFor[agentconfig.Config]()
			for i := range typ.NumField() {
				key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
				if noTemplate && key == "template" {
					Expect(got).NotTo(HaveKey(key))
					continue
				}
				Expect(got).To(HaveKey(key), "add the new Config field %s to compat.FullConfig", typ.Field(i).Name)
				Expect(got[key]).NotTo(BeZero(), "compat.FullConfig leaves %s zero", typ.Field(i).Name)
				if m, ok := got[key].(map[string]any); ok {
					Expect(m).NotTo(BeEmpty(), "compat.FullConfig leaves %s empty", typ.Field(i).Name)
				}
			}
		}
	})
	It("records the full configs", func() {
		for _, n := range []string{"full", "full-no-template"} {
			Expect(filepath.Join(dir, "agent_configs", n+".json")).To(BeAnExistingFile())
		}
	})
	It("fails when a stored config decodes differently than recorded", func() {
		entries, err := filepath.Glob(filepath.Join(dir, "agent_configs", "*.decoded.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).NotTo(BeEmpty())
		Expect(os.WriteFile(entries[0], []byte(`{"instructions":"something the reader does not produce"}`), 0o600)).To(Succeed())
		DeferCleanup(func() { _ = os.RemoveAll(filepath.Join(dir, "agent_configs")) })
		Expect(compat.CheckAgentConfigs(dir)).NotTo(Succeed())
	})
	It("accepts a key that only head produces", func() {
		ok, err := compat.JSONContains([]byte(`{"a":1,"new":{"x":0},"l":[{"b":2,"c":3}]}`), []byte(`{"a":1,"l":[{"b":2}]}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		ok, err = compat.JSONContains([]byte(`{"a":1}`), []byte(`{"a":1,"gone":2}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})
	It("refuses to overwrite a recorded folder", func(ctx SpecContext) {
		Expect(compat.Record(ctx, dir, "again")).NotTo(Succeed())
	})
})
