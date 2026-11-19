// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"os"
	"regexp"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/preflight"
)

// The worker container shape that the chart's admission policy admits (execution.substrate.workerPolicy.workerShapes) is the
// shape that Substrate sets (workerpool_apply.go:377-468). The vendored excerpt is the source at the pinned commit. When the
// pin moves, this spec fails until a person fetches the excerpt again, so a change of Substrate's shape never goes unseen
// (ADR 0120 section 6, issue 1089).
var _ = Describe("The worker shape of the admission policy", func() {
	const excerpt = "testdata/substrate_workerpool_apply_shape.go.txt"

	read := func(path string) string {
		GinkgoHelper()
		b, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}
	capsOf := func(src, name string) []string {
		GinkgoHelper()
		m := regexp.MustCompile(`(?s)var ` + name + ` = \[\]corev1\.Capability\{(.*?)\}`).FindStringSubmatch(src)
		Expect(m).NotTo(BeNil(), name+" is in the excerpt")
		var out []string
		for _, q := range regexp.MustCompile(`"([A-Z_]+)"`).FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
		sort.Strings(out)
		return out
	}

	It("names the pinned Substrate commit", func() {
		Expect(read(excerpt)).To(ContainSubstring("at commit "+preflight.SubstratePinnedCommit),
			"the Substrate pin moved: fetch lines 377-468 of workerpool_apply.go at the new commit into "+excerpt+
				", compare the shape with the chart values (workerShapes), and update the commit in the excerpt header")
	})

	It("has the capabilities of each class, drop ALL, root, and no privilege escalation", func() {
		src := read(excerpt)
		var values struct {
			Execution struct {
				Substrate struct {
					WorkerPolicy struct {
						WorkerShapes map[string]struct {
							Capabilities []string `json:"capabilities"`
						} `json:"workerShapes"`
					} `json:"workerPolicy"`
				} `json:"substrate"`
			} `json:"execution"`
		}
		Expect(yaml.Unmarshal([]byte(read("../../charts/koinon/values.yaml")), &values)).To(Succeed())
		shapes := values.Execution.Substrate.WorkerPolicy.WorkerShapes
		for class, source := range map[string]string{"gvisor": "workerGvisorCapabilities", "microvm": "workerMicroVMCapabilities"} {
			got := append([]string(nil), shapes[class].Capabilities...)
			sort.Strings(got)
			Expect(got).To(Equal(capsOf(src, source)), "workerShapes."+class+".capabilities must equal "+source+" of Substrate")
		}
		for _, call := range []string{`WithRunAsUser(0)`, `WithRunAsGroup(0)`, `WithPrivileged(false)`,
			`WithAllowPrivilegeEscalation(false)`, `WithDrop("ALL")`} {
			Expect(strings.Count(src, call)).To(BeNumerically(">=", 1), "Substrate sets "+call+" and the policy requires it")
		}
		Expect(src).To(ContainSubstring("SeccompProfileTypeUnconfined"))
		Expect(src).To(ContainSubstring("AppArmorProfileTypeUnconfined"))
	})
})
