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
		Expect(judge("pkg", nil, "", "all").verdict).To(Equal(notRun))
	})

	It("is PASS (partly skipped) when a spec passed and another skipped for docker", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "", "all")
		Expect(r.verdict).To(Equal(partial))
		Expect(r.why).To(Equal("PASS (partly skipped: docker)"))
		Expect(r.incomplete).To(BeTrue())
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("is PASS when a spec passed and nothing skipped", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeIt, types.SpecStatePassed, "")), "", "all")
		Expect(r.verdict).To(Equal(pass))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is partial but not incomplete when only an opt-in requirement skipped", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "koinon:requires=openshell: not set"),
		), "", "docker,git,helm")
		Expect(r.verdict).To(Equal(partial))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is FAIL when a partly skipped suite lacks a requirement KOINON_REQUIRE lists", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "docker", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is SKIPPED but not incomplete when only an opt-in requirement is missing", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "koinon:requires=e2e: KOINON_E2E is not set")), "", "docker,git,helm")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.incomplete).To(BeFalse())
	})

	It("is FAIL when a spec failed, whatever passed", func() {
		r := judge("pkg", report(false,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateFailed, "boom"),
		), "", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is SKIPPED when a BeforeSuite skipped for a declared requirement", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "", "all")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"docker"}))
	})

	It("ignores the skips Ginkgo propagates from a skipped BeforeAll (test/escape)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeAll, types.SpecStateSkipped, "koinon:requires=envtest: KUBEBUILDER_ASSETS is not set"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "Spec skipped because Skip() was called in BeforeAll"),
		), "", "all")
		Expect(r.verdict).To(Equal(skipped))
		Expect(r.requires).To(Equal([]string{"envtest"}))
	})

	It("is FAIL when KOINON_REQUIRE lists the missing requirement", func() {
		r := judge("pkg", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker)), "envtest,docker", "all")
		Expect(r.verdict).To(Equal(fail))
	})

	It("is NOT RUN when everything skipped without a declared requirement (chart-smoke's 0 of 56)", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, "Docker is not available"),
			spec(types.NodeTypeIt, types.SpecStateSkipped, ""),
		), "", "all")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when one undeclared skip sits beside a declared one", func() {
		r := judge("pkg", report(true,
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "not wired yet"),
		), "", "all")
		Expect(r.verdict).To(Equal(notRun))
	})

	It("is NOT RUN when the suite ran no spec at all", func() {
		Expect(judge("pkg", report(true), "", "all").verdict).To(Equal(notRun))
	})

	It("counts the specs of every parallel process", func() {
		rs := append(report(true, spec(types.NodeTypeIt, types.SpecStatePassed, "")),
			report(true, spec(types.NodeTypeIt, types.SpecStatePassed, ""))...)
		Expect(judge("pkg", rs, "", "all").passed).To(Equal(2))
	})
})

var _ = Describe("summarize", func() {
	It("never calls a run with a skipped suite a pass, and exits 3", func() {
		var out bytes.Buffer
		code := summarize(&out, "go-test", []result{
			{dir: "a", verdict: pass, passed: 3},
			{dir: "b", verdict: skipped, requires: []string{"docker"}, incomplete: true, expected: []string{"docker"}},
		})
		Expect(code).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("SKIPPED, NOT PASSED: missing docker"))
		Expect(out.String()).To(ContainSubstring("  SKIPPED  b (docker)"))
		Expect(out.String()).To(ContainSubstring("INCOMPLETE (missing docker)"))
		Expect(out.String()).NotTo(ContainSubstring("PASS "))
	})

	It("exits 3 for a partly skipped suite with an expected requirement", func() {
		var out bytes.Buffer
		r := judge("a", report(true,
			spec(types.NodeTypeIt, types.SpecStatePassed, ""),
			spec(types.NodeTypeIt, types.SpecStateSkipped, docker),
		), "", "docker,git")
		Expect(r.why).To(Equal("PASS (partly skipped: docker)"))
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("  PARTLY   a (docker)"))
		Expect(out.String()).To(ContainSubstring("INCOMPLETE (missing docker)"))
		Expect(out.String()).To(ContainSubstring("1 partly skipped"))
	})

	It("lists opt-in partial and skipped suites and exits 0", func() {
		var out bytes.Buffer
		results := []result{
			{dir: "a", verdict: pass, passed: 3},
			{dir: "b", verdict: partial, passed: 1, requires: []string{"openshell"}},
			{dir: "c", verdict: skipped, requires: []string{"e2e"}},
		}
		Expect(summarize(&out, "go-test", results)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("opt-in suites not run here: b (openshell), c (e2e)"))
		Expect(out.String()).NotTo(ContainSubstring("NOT PASSED"))
		Expect(out.String()).To(ContainSubstring("go-test verdict: ok:"))
	})

	It("names only the expected requirements in the banner", func() {
		var out bytes.Buffer
		r := judge("a", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker),
			spec(types.NodeTypeIt, types.SpecStateSkipped, "koinon:requires=git: git is not on PATH")), "", "docker,git,helm")
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
		Expect(out.String()).To(ContainSubstring("(missing docker,git)"))
	})

	It("exits 3 for a SKIPPED suite lacking an expected docker", func() {
		var out bytes.Buffer
		r := judge("a", report(true, spec(types.NodeTypeBeforeSuite, types.SpecStateSkipped, docker)), "", "docker,git,helm")
		Expect(summarize(&out, "go-test", []result{r})).To(Equal(3))
	})

	It("fails a run with a NOT RUN suite", func() {
		var out bytes.Buffer
		Expect(summarize(&out, "go-test", []result{{dir: "a", verdict: notRun, why: "no report"}})).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("NOT RUN  a: no report"))
	})

	It("lets FAIL beat incomplete", func() {
		var out bytes.Buffer
		code := summarize(&out, "go-test", []result{
			{dir: "a", verdict: fail, why: "boom"},
			{dir: "b", verdict: skipped, requires: []string{"docker"}, incomplete: true, expected: []string{"docker"}},
		})
		Expect(code).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("FAILED"))
	})
})
