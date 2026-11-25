// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestVapcheck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "vapcheck")
}

func check(fixture string) (int, []string) {
	f, err := os.Open(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()
	n, problems, err := Check(f)
	Expect(err).NotTo(HaveOccurred())
	return n, problems
}

var _ = Describe("Check", func() {
	It("accepts the gateway key guard as the chart renders it now", func() {
		n, problems := check("good-gateway-guard.yaml")
		Expect(n).To(Equal(1))
		Expect(problems).To(BeEmpty())
	})

	// The proof of issue 1313: the old template line renders a YAML string where a CEL string
	// literal belongs. helm lint passes it; the API server (and this check) refuse it.
	It("refuses the old gateway key guard writer variable", func() {
		n, problems := check("old-gateway-guard-writer.yaml")
		Expect(n).To(Equal(1))
		Expect(problems).To(HaveLen(1))
		Expect(problems[0]).To(ContainSubstring("koinon-koinon-gateway-keys"))
		Expect(problems[0]).To(ContainSubstring("spec.variables[0] (writer)"))
		Expect(problems[0]).To(ContainSubstring("compilation failed"))
	})

	It("names every place with a bad expression and skips other kinds", func() {
		n, problems := check("bad-each-field.yaml")
		Expect(n).To(Equal(1))
		joined := strings.Join(problems, "\n")
		for _, where := range []string{
			"spec.matchConditions[0] (bad-match)",
			"spec.validations[0].expression",
			"spec.validations[1].expression",
			"spec.validations[1].messageExpression",
			"spec.auditAnnotations[0] (bad)",
		} {
			Expect(joined).To(ContainSubstring(where))
		}
		Expect(problems).To(HaveLen(5))
	})

	It("compiles for the oldest Kubernetes version the chart supports", func() {
		_, problems := check("newer-library.yaml")
		Expect(problems).To(HaveLen(1))
		Expect(problems[0]).To(ContainSubstring("undeclared reference"))
	})

	It("declares authorizer and params as the API server does", func() {
		n, problems := check("declarations.yaml")
		Expect(n).To(Equal(1)) // the policy of another API group is skipped
		Expect(problems).To(HaveLen(1))
		Expect(problems[0]).To(ContainSubstring("spec.validations[0].messageExpression"))
		Expect(problems[0]).To(ContainSubstring("authorizer"))

		_, problems = check("params-without-kind.yaml")
		Expect(problems).To(HaveLen(1))
		Expect(problems[0]).To(ContainSubstring("params"))
	})

	It("reads input that is not YAML as an error", func() {
		_, _, err := Check(strings.NewReader("a: [unterminated"))
		Expect(err).To(HaveOccurred())
	})
})
