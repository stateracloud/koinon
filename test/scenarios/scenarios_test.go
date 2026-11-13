// SPDX-License-Identifier: Apache-2.0

package scenarios_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	fakemodelscripts "github.com/stateracloud/koinon/test/fakemodel"
	"github.com/stateracloud/koinon/test/scenarios"
)

var _ = Describe("the repo-helper scenario", func() {
	var script fakemodel.Script

	BeforeEach(func() {
		all, err := fakemodel.LoadFS(fakemodelscripts.FS, "scripts")
		Expect(err).NotTo(HaveOccurred())
		for _, s := range all {
			if s.Model == scenarios.PullRequest.Model {
				script = s
			}
		}
		Expect(script.Model).To(Equal(scenarios.PullRequest.Model), "the script exists")
	})

	// toolTurn is the script's turn that proposes the pull request.
	toolTurn := func(script fakemodel.Script, name string) (fakemodel.Turn, fakemodel.ToolCall) {
		for _, t := range script.Turns {
			for _, c := range t.ToolCalls {
				if c.Name == name {
					return t, c
				}
			}
		}
		Fail("the script has no turn calling " + name)
		return fakemodel.Turn{}, fakemodel.ToolCall{}
	}

	// These specs keep the data honest: when a script changes, the scenario (and so every test
	// that asserts it) changes with it, in the same commit.
	It("proposes the branch, title and file the scenario expects", func() {
		p := scenarios.PullRequest
		turn, call := toolTurn(script, p.ParkingTool)
		Expect(call.Arguments).To(HaveKeyWithValue("branch", p.Suggestion))
		Expect(call.Arguments).To(HaveKeyWithValue("title", p.Title))
		Expect(call.Arguments["files"]).To(ConsistOf(SatisfyAll(
			HaveKeyWithValue("path", p.File), HaveKeyWithValue("content", p.Content))))
		Expect(turn.Usage.InputTokens).To(BeEquivalentTo(p.Ledger.StreamedInputTokens),
			"the tool-call turn's reported input is what the ledger's totals wait for")
	})

	It("asks for the tracker's write tool the scenario names", func() {
		p := scenarios.PullRequest
		_, call := toolTurn(script, p.TrackerAskTool)
		raw, err := json.Marshal(call.Arguments)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal(p.TrackerAskArgs), "the e2e compares the approval's arguments to this exactly")
	})

	It("reads from the tracker, then calls its write tool with the arguments the guard's case names", func() {
		p := scenarios.PullRequest
		read, _ := toolTurn(script, p.TrackerReadTool)
		Expect(read.When.UserContains).To(Equal(p.TrackerGuardPrompt))
		write, call := toolTurn(script, p.TrackerGuardTool)
		Expect(write.When.UserContains).To(Equal(p.TrackerGuardPrompt))
		Expect(write.When.ToolResult).To(Equal(p.TrackerReadTool), "the write comes after the read: both legs first")
		raw, err := json.Marshal(call.Arguments)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal(p.TrackerGuardArgs), "the e2e compares the approval's arguments to this exactly")
	})

	It("files the branch under the agent's namespace", func() {
		got, err := scenarios.PullRequest.Branch("Repo Helper", "0194f2c0-1111-7000-8000-00000000abcd")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("koinon/repo-helper-0000abcd/add-hello-md"))
	})
})

var _ = Describe("the ask-then-pull-request scenario", func() {
	It("asks the question the scenario names, with its choices and default, then proposes the pull request", func() {
		g := scenarios.GiteaAsk
		all, err := fakemodel.LoadFS(fakemodelscripts.FS, "scripts")
		Expect(err).NotTo(HaveOccurred())
		var script fakemodel.Script
		for _, s := range all {
			if s.Model == g.Model {
				script = s
			}
		}
		Expect(script.Model).To(Equal(g.Model), "the script exists")
		var ask, pr *fakemodel.Turn
		for i, t := range script.Turns {
			for _, c := range t.ToolCalls {
				switch c.Name {
				case g.AskTool:
					ask = &script.Turns[i]
					Expect(c.Arguments).To(HaveKeyWithValue("question", g.Question))
					Expect(c.Arguments).To(HaveKeyWithValue("default", g.Default))
					Expect(c.Arguments["choices"]).To(ConsistOf(g.Choices[0], g.Choices[1]))
				case g.PRTool:
					pr = &script.Turns[i]
				}
			}
		}
		Expect(ask).NotTo(BeNil(), "a turn calls "+g.AskTool)
		Expect(pr).NotTo(BeNil(), "a turn calls "+g.PRTool)
		Expect(pr.When.UserContains).To(Equal("the person answered"), "the pull request comes after the answer")
		Expect(g.Choices).To(ContainElement(g.Answer), "the scripted answer is one of the choices")
		Expect(g.Choices).To(ContainElement(g.Default))
	})
})

var _ = Describe("the scenario's ledger checks", func() {
	l := scenarios.PullRequest.Ledger
	refused := scenarios.Entry{UpstreamStatus: 400, CostZero: true, ChargeWaived: "provider type openai does not bill refused requests"}
	streamed := scenarios.Entry{Input: 1200, Output: 60}

	It("accepts a waived refusal and a streamed request", func() {
		Expect(l.CheckRefused(refused)).To(Succeed())
		Expect(l.CheckStreamed(streamed)).To(Succeed())
	})

	It("refuses a refusal that is estimated, charged or not waived", func() {
		for _, bad := range []scenarios.Entry{
			{UpstreamStatus: 400, CostZero: true},
			{UpstreamStatus: 400, CostZero: true, ChargeWaived: "x", Estimated: true},
			{UpstreamStatus: 400, CostZero: true, ChargeWaived: "x", Input: 5},
			{UpstreamStatus: 400, ChargeWaived: "x"},
			{CostZero: true, ChargeWaived: "x"},
		} {
			Expect(l.CheckRefused(bad)).NotTo(Succeed(), "%+v", bad)
		}
	})

	It("refuses a streamed request that is estimated or reports nothing", func() {
		Expect(l.CheckStreamed(scenarios.Entry{Input: 1200, Estimated: true})).NotTo(Succeed())
		Expect(l.CheckStreamed(scenarios.Entry{})).NotTo(Succeed())
	})

	It("does not call a run's totals settled on the refused entry alone", func() {
		Expect(l.Settled(scenarios.Totals{Requests: 1})).To(BeFalse())
		Expect(l.Settled(scenarios.Totals{Requests: 3, InputTokens: 1200, OutputTokens: 60})).To(BeTrue())
		err := l.CheckTotals(scenarios.Totals{Requests: 3, EstimatedRequests: 1, InputTokens: 1200})
		Expect(err).To(MatchError(ContainSubstring("estimated")))
		Expect(strings.Count(l.CheckTotals(scenarios.Totals{OutputTokens: 5000}).Error(), ";")).To(Equal(1))
	})
})
