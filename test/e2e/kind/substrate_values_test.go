// SPDX-License-Identifier: Apache-2.0

package kind_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

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

	// run.sh fills the placeholders with kind/values-rewrite.sh. The image cache maps the registry host of the harness image
	// reference to itself: the key of the mirror must be that host, or the reference stays unrewritten (run 45841952354).
	It("keeps the cache mirror key equal to the registry host of the harness image after the run.sh rewrite", func() {
		if _, err := exec.LookPath("helm"); err != nil {
			Skip("helm is not installed")
		}
		out := filepath.Join(GinkgoT().TempDir(), "values.yaml")
		rewritten, err := exec.Command("bash", "values-rewrite.sh", "substrate-values.yaml", "localhost:5001/koinon-e1", "10.9.0.0/16").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(rewritten))
		Expect(os.WriteFile(out, rewritten, 0o600)).To(Succeed())
		Expect(string(rewritten)).To(ContainSubstring("10.9.0.0/16"))
		Expect(string(rewritten)).NotTo(ContainSubstring("172.18.0.0/16"))

		rendered, err := exec.Command("helm", "template", "koinon", "../../../charts/koinon", "-n", "koinon", "-f", out).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(rendered))
		image := regexp.MustCompile(`(?m)image: "?(localhost:5001/koinon-e1/sandbox-substrate:[^"\s]+)`).FindStringSubmatch(string(rendered))
		Expect(image).NotTo(BeNil(), "the harness image reference in the render")
		host := strings.SplitN(image[1], "/", 2)[0]
		Expect(host).To(Equal("localhost:5001"))
		mirror := regexp.MustCompile(`(?m)- "(localhost:5001)=([^"]+)"`).FindStringSubmatch(string(rendered))
		Expect(mirror).NotTo(BeNil(), "the cache mirror entry of the registry host")
		Expect(mirror[1]).To(Equal(host), "the mirror key is the registry host of the harness image")
		Expect(mirror[2]).NotTo(MatchRegexp(`/localhost:`), "no colon in the mirror path")
	})
})
