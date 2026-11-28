// SPDX-License-Identifier: Apache-2.0

package kindvalues_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/faketls"
	"github.com/stateracloud/koinon/test/e2e/kindvalues"
)

func TestKindValues(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "kindvalues")
}

var _ = Describe("FillEgressCA (#1487)", func() {
	var ca []byte
	BeforeEach(func() {
		set, err := faketls.Make("kindvalues test CA", []string{"localhost"})
		Expect(err).NotTo(HaveOccurred())
		ca = set.CA
	})

	It("puts the CA into gateway.mcp.koinon.egress.caBundle of the kind values file", func() {
		in, err := os.ReadFile("../kind/koinon-values.yaml")
		Expect(err).NotTo(HaveOccurred())
		out, err := kindvalues.FillEgressCA(in, ca)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).NotTo(ContainSubstring("@E2E_EGRESS_CA@"))

		var v struct {
			Gateway struct {
				MCP struct {
					Koinon struct {
						Egress struct {
							Enabled  bool   `yaml:"enabled"`
							CABundle string `yaml:"caBundle"`
						} `yaml:"egress"`
					} `yaml:"koinon"`
				} `yaml:"mcp"`
			} `yaml:"gateway"`
		}
		Expect(yaml.Unmarshal(out, &v)).To(Succeed())
		Expect(v.Gateway.MCP.Koinon.Egress.Enabled).To(BeTrue())
		Expect(v.Gateway.MCP.Koinon.Egress.CABundle).To(Equal(string(ca)))
	})

	It("keeps the indentation and the rest of the file", func() {
		in := []byte("a:\n  b:\n    caBundle: \"@E2E_EGRESS_CA@\"\n  c: 1\n")
		out, err := kindvalues.FillEgressCA(in, ca)
		Expect(err).NotTo(HaveOccurred())
		var v map[string]map[string]any
		Expect(yaml.Unmarshal(out, &v)).To(Succeed())
		Expect(v["a"]["c"]).To(Equal(1))
		Expect(v["a"]["b"]).To(HaveKeyWithValue("caBundle", string(ca)))
	})

	It("refuses values without the placeholder line", func() {
		_, err := kindvalues.FillEgressCA([]byte("a: 1\n"), ca)
		Expect(err).To(MatchError(kindvalues.ErrNoEgressCAPlaceholder))
	})

	It("refuses two placeholder lines", func() {
		_, err := kindvalues.FillEgressCA([]byte("a: \"@E2E_EGRESS_CA@\"\nb: \"@E2E_EGRESS_CA@\"\n"), ca)
		Expect(err).To(MatchError(ContainSubstring("want one")))
	})

	It("refuses a CA without a PEM certificate", func() {
		_, err := kindvalues.FillEgressCA([]byte("caBundle: \"@E2E_EGRESS_CA@\"\n"), []byte("@E2E_EGRESS_CA@"))
		Expect(err).To(MatchError(ContainSubstring("no PEM certificate")))
	})
})
