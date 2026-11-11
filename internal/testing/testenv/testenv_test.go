// SPDX-License-Identifier: Apache-2.0

package testenv

import (
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

func TestTestenv(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testenv")
}

func missing(name string) Requirement {
	return Requirement{Name: name, check: func() error { return errors.New("not here") }}
}

var _ = Describe("decide", func() {
	// These specs name their fake requirements "docker"; a KOINON_TESTENV_UNAVAILABLE=docker run
	// (the no-Docker gate) must not mark them missing.
	BeforeEach(func() { GinkgoT().Setenv(UnavailableVar, "") })

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

	It("Binary needs the executable on PATH", func() {
		Expect(Binary("sh").Err()).To(Succeed())
		err := Binary("koinon-no-such-binary").Err()
		Expect(err).To(MatchError(ContainSubstring("not on PATH")))
		Expect(Binary("helm").Name).To(Equal("helm"))
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

// fakeT records what RequireT does; the embedded nil TB is never called otherwise.
type fakeT struct {
	testing.TB
	skipped, failed string
}

func (f *fakeT) Helper()        {}
func (f *fakeT) Skip(a ...any)  { f.skipped = fmt.Sprint(a...) }
func (f *fakeT) Fatal(a ...any) { f.failed = fmt.Sprint(a...) }

var _ = Describe("RequireT", func() {
	BeforeEach(func() { GinkgoT().Setenv(UnavailableVar, "") })

	It("skips, fails under KOINON_REQUIRE, and does nothing when met", func() {
		f := &fakeT{}
		GinkgoT().Setenv(RequireVar, "")
		RequireT(f, missing("helm"))
		Expect(f.skipped).To(HavePrefix("koinon:requires=helm: not here"))
		Expect(f.failed).To(BeEmpty())

		f = &fakeT{}
		GinkgoT().Setenv(RequireVar, "helm")
		RequireT(f, missing("helm"))
		Expect(f.failed).To(ContainSubstring("KOINON_REQUIRE=helm makes this a failure"))
		Expect(f.skipped).To(BeEmpty())

		f = &fakeT{}
		RequireT(f, Requirement{Name: "helm", check: func() error { return nil }})
		Expect(f.skipped + f.failed).To(BeEmpty())
	})

	It("BinaryNamed keeps the name apart from the executable", func() {
		r := BinaryNamed("helm", "koinon-no-such-binary")
		Expect(r.Name).To(Equal("helm"))
		Expect(r.Err()).To(MatchError(ContainSubstring("koinon-no-such-binary is not on PATH")))
	})
})

var _ = Describe("dockerCheck", func() {
	It("fails within the deadline when the provider lookup hangs", func() {
		release := make(chan struct{})
		DeferCleanup(func() { close(release) })
		hang := func(...testcontainers.GenericProviderOption) (testcontainers.GenericProvider, error) {
			<-release
			return nil, errors.New("late")
		}
		start := time.Now()
		err := dockerCheck(100*time.Millisecond, hang)
		Expect(err).To(MatchError(ContainSubstring("docker is not available: no answer within")))
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
	})

	It("reports a lookup error and a panic as unavailable", func() {
		fail := func(...testcontainers.GenericProviderOption) (testcontainers.GenericProvider, error) {
			return nil, errors.New("no socket")
		}
		Expect(dockerCheck(time.Second, fail)).To(MatchError(ContainSubstring("no socket")))
		boom := func(...testcontainers.GenericProviderOption) (testcontainers.GenericProvider, error) { panic("boom") }
		Expect(dockerCheck(time.Second, boom)).To(MatchError(ContainSubstring("panicked: boom")))
	})
})
