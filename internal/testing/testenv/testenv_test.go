// SPDX-License-Identifier: Apache-2.0

package testenv

import (
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTestenv(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testenv")
}

func missing(name string) Requirement {
	return Requirement{Name: name, check: func() error { return errors.New("not here") }}
}

var _ = Describe("decide", func() {
	It("does nothing when the requirement is met", func() {
		met := Requirement{Name: "docker", check: func() error { return nil }}
		skip, msg := decide(met, "docker")
		Expect(skip).To(BeFalse())
		Expect(msg).To(BeEmpty())
	})

	DescribeTable("skips unless KOINON_REQUIRE lists the requirement",
		func(required string, skip bool) {
			gotSkip, msg := decide(missing("docker"), required)
			Expect(gotSkip).To(Equal(skip))
			Expect(msg).To(HavePrefix("koinon:requires=docker: not here"))
		},
		Entry("unset", "", true),
		Entry("another requirement", "envtest", true),
		Entry("listed", "docker", false),
		Entry("listed among others", "envtest, docker", false),
		Entry("all", "all", false),
		Entry("a longer name is not a match", "dockerd", true),
	)
})

var _ = Describe("requirements", func() {
	It("treats a requirement listed in KOINON_TESTENV_UNAVAILABLE as missing", func() {
		met := Requirement{Name: "docker", check: func() error { return nil }}
		GinkgoT().Setenv(UnavailableVar, "envtest,docker")
		Expect(met.Err()).To(MatchError(ContainSubstring("marks it unavailable")))
		GinkgoT().Setenv(UnavailableVar, "envtest")
		Expect(met.Err()).To(Succeed())
	})

	It("Var needs a non-empty variable", func() {
		GinkgoT().Setenv("KOINON_TESTENV_PROBE", "")
		Expect(Var("probe", "KOINON_TESTENV_PROBE").Err()).To(HaveOccurred())
		GinkgoT().Setenv("KOINON_TESTENV_PROBE", "x")
		Expect(Var("probe", "KOINON_TESTENV_PROBE").Err()).To(Succeed())
	})

	It("Envtest needs KUBEBUILDER_ASSETS to be a directory", func() {
		GinkgoT().Setenv("KUBEBUILDER_ASSETS", "")
		Expect(Envtest.Err()).To(HaveOccurred())
		GinkgoT().Setenv("KUBEBUILDER_ASSETS", GinkgoT().TempDir()+"/missing")
		Expect(Envtest.Err()).To(HaveOccurred())
		GinkgoT().Setenv("KUBEBUILDER_ASSETS", GinkgoT().TempDir())
		Expect(Envtest.Err()).To(Succeed())
	})
})
