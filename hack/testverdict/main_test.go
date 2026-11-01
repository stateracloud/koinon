// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
)

func TestTestverdict(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testverdict")
}

func spec(kind types.NodeType, state types.SpecState, msg string) types.SpecReport {
	return types.SpecReport{LeafNodeType: kind, LeafNodeText: "x", State: state, Failure: types.Failure{Message: msg}}
}

func report(ok bool, specs ...types.SpecReport) []types.Report {
	return []types.Report{{SuitePath: "/r/pkg", SuiteSucceeded: ok, SpecReports: specs}}
}

const docker = "koinon:requires=docker: docker is not available: no socket"

var _ = Describe("judge", func() {
	It("is NOT RUN without a report (the ginkgo -p timeout of #507)", func() {
		Expect(judge("pkg", nil, "").verdict).To(Equal(notRun))
	})

	It("is PASS when a spec passed", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "")
		Expect(r.verdict).To(Equal(pass))
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("is FAIL when a spec failed, whatever passed", func() {
		r := judge("pkg", report(false,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateFailed, "boom"),
		), "")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is SKIPPED when a BeforeSuite skipped for a declared requirement", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("ignores the skips Ginkgo propagates from a skipped BeforeAll (test/escape)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeAll, types.SpecStateSkipped, "koinon:requires=envtest: KUBEBUILDER_ASSETS is not set"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "Spec skipped because Skip() was called in BeforeAll"),
		), "")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"envtest"}))
	})

	It("is FAIL when KOINON_REQUIRE lists the missing requirement", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker)), "envtest,docker")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is NOT RUN when everything skipped without a declared requirement (chart-smoke's 0 of 56)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "Docker is not available"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when one undeclared skip sits beside a declared one", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "not wired yet"),
		), "")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when the suite ran no spec at all", func() {
		Expect(judge("pkg", report(true), "").verdict).To(Equal(notRun))
	})

	It("counts the specs of every parallel process", func() {
		rs := append(report(true, spec(types.NodeTypeIt, types.SpecStatePassed, "")),
			report(true, spec(types.NodeTypeIt, types.SpecStatePassed, ""))...)
		Expect(judge("pkg", rs, "").passed).To(Equal(2))
	})
})

var _ = Describe("summarize", func() {
	It("never calls a run with a skipped suite a pass, but lets it through", func() {
		var out bytes.Buffer
		ok := summarize(&out, "go-test", []result{
			{dir: "a", verdict: pass, passed: 3},
			{dir: "b", verdict: skipped, requires: []string{"docker"}},
		})
		Expect(ok).To(BeTrue())
		Expect(out.String()).To(ContainSubstring("SKIPPED, NOT PASSED: missing docker"))
		Expect(out.String()).To(ContainSubstring("INCOMPLETE"))
		Expect(out.String()).NotTo(ContainSubstring("PASS "))
	})

	It("fails a run with a NOT RUN suite", func() {
		var out bytes.Buffer
		Expect(summarize(&out, "go-test", []result{{dir: "a", verdict: notRun, why: "no report"}})).To(BeFalse())
		Expect(out.String()).To(ContainSubstring("NOT RUN  a: no report"))
	})
})
