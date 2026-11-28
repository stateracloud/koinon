// SPDX-License-Identifier: Apache-2.0

package kind_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/test/e2e/kindvalues"
)

// filledSubstrateValues writes a copy of substrate-values.yaml with the egress CA placeholder filled with a throwaway
// test CA, as test/e2e/run.sh does with a real one (fill-egress-ca), and returns its path.
func filledSubstrateValues() string {
	GinkgoHelper()
	raw, err := os.ReadFile("substrate-values.yaml")
	Expect(err).NotTo(HaveOccurred())
	filled, err := kindvalues.FillEgressCA(raw, testCAPEM())
	Expect(err).NotTo(HaveOccurred())
	out := filepath.Join(GinkgoT().TempDir(), "substrate-values.yaml")
	Expect(os.WriteFile(out, filled, 0o600)).To(Succeed())
	return out
}

func testCAPEM() []byte {
	GinkgoHelper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "koinon-test-not-a-real-ca"}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

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
		out, err := exec.Command("helm", "template", "koinon", "../../../charts/koinon", "-n", "koinon", "-f", filledSubstrateValues()).CombinedOutput()
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
		filled, err := kindvalues.FillEgressCA(rewritten, testCAPEM())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(out, filled, 0o600)).To(Succeed())
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
