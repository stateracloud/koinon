// SPDX-License-Identifier: Apache-2.0

package kind_test

import (
	"os"
	"os/exec"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

func readValues(name string) map[string]any {
	GinkgoHelper()
	raw, err := os.ReadFile(name)
	Expect(err).NotTo(HaveOccurred())
	var v map[string]any
	Expect(yaml.Unmarshal(raw, &v)).To(Succeed())
	return v
}

// The substrate leg of the kind e2e (issue #1091) installs substrate-values.yaml in place of koinon-values.yaml. What the
// quickstart needs from the fakes must stay the same in both, and run.sh must find its two placeholders.
var _ = Describe("substrate-values.yaml", func() {
	var sub, osh map[string]any

	BeforeEach(func() {
		sub = readValues("substrate-values.yaml")
		osh = readValues("koinon-values.yaml")
	})

	It("selects the substrate driver at the dev tier with the OpenShell-only features off", func() {
		driver := sub["execution"].(map[string]any)
		Expect(driver["driver"]).To(Equal("substrate"))
		Expect(driver["substrate"].(map[string]any)["defaultTier"]).To(Equal("dev"))
		for _, k := range []string{"sandboxObserver", "sandboxNetworkPolicy", "sandboxLimitRange"} {
			Expect(sub[k].(map[string]any)["enabled"]).To(BeFalse(), k)
		}
		Expect(sub).NotTo(HaveKey("openshell"))
	})

	It("keeps the fakes and the image settings of the OpenShell values", func() {
		for _, k := range []string{"image", "postgres", "gateway", "egress", "networkPolicy", "github"} {
			Expect(sub[k]).To(Equal(osh[k]), k)
		}
	})

	It("carries the placeholders that test/e2e/run.sh replaces", func() {
		raw, err := os.ReadFile("substrate-values.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("localhost:5001"))
		Expect(regexp.MustCompile(`digest: sha256:0{64}\n`).Match(raw)).To(BeTrue(), "the all-zero harness digest")
	})

	It("renders with the chart", func() {
		if _, err := exec.LookPath("helm"); err != nil {
			Skip("helm is not installed")
		}
		out, err := exec.Command("helm", "template", "koinon", "../../../charts/koinon", "-n", "koinon", "-f", "substrate-values.yaml").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(string(out)).To(ContainSubstring("ate-workers"))
	})
})
