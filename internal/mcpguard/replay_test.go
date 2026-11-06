// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	g "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The replay suite (#352, ADR 0091): corpora of MCP traffic replayed through the guard's decision
// path, asserted per sink class (corpus_test.go has the format and the replay). The corpora here are
// HAND-BUILT by testdata/replay/gen_corpus.py, not recorded: they prove the harness and pin the
// decision table, they measure nothing. A recorded, scrubbed corpus is measured with
// KOINON_LEAKGUARD_CORPUS (docs/dev/real-model-tests.md).

const (
	benignCorpus = "testdata/replay/handbuilt-benign.jsonl"
	leakCorpus   = "testdata/replay/handbuilt-leaks.jsonl"
	// CorpusEnv names a recorded corpus to measure.
	corpusEnv = "KOINON_LEAKGUARD_CORPUS"
)

func mustCorpus(path string) []corpusLine {
	g.GinkgoHelper()
	lines, err := readCorpus(path)
	Expect(err).NotTo(HaveOccurred())
	Expect(lines).NotTo(BeEmpty())
	return lines
}

func mustReplay(ctx g.SpecContext, lines []corpusLine, o replayOptions) []replayed {
	g.GinkgoHelper()
	calls, err := replayAll(ctx, lines, o)
	Expect(err).NotTo(HaveOccurred())
	return calls
}

// where names a call in a failure message.
func where(c replayed) string {
	return c.line.Run + " #" + strconv.Itoa(c.line.Seq) + " " + c.line.Server + "/" + c.line.Tool + " (" + string(c.class) + ")"
}

var _ = g.Describe("Replay (#352, ADR 0091)", func() {
	g.Describe("the hand-built benign corpus, asserted per sink class", func() {
		var calls []replayed
		g.BeforeEach(func(ctx g.SpecContext) {
			calls = mustReplay(ctx, mustCorpus(benignCorpus), replayOptions{})
		})

		g.It("passes every none and internal call, whatever it carries and whatever the legs", func() {
			byTool := map[string]int{}
			for _, c := range calls {
				if c.class != sinkclass.None && c.class != sinkclass.Internal {
					continue
				}
				Expect(c.verdict.Action).To(Equal(Pass), where(c))
				Expect(c.verdict.Reason).To(Equal(ReasonInternal), where(c))
				byTool[c.line.Server+"/"+c.line.Tool]++
			}
			// Each way to be internal or none is in the corpus: a code-host write into the org's own
			// private repository, a fixed internal tool, a fetch of a trusted host and a read.
			Expect(byTool).To(HaveKey("gitea/create_issue_comment"))
			Expect(byTool).To(HaveKey("gitea/create_issue"))
			Expect(byTool).To(HaveKey("gitea/create_pull_request"))
			Expect(byTool).To(HaveKey("tickets/create_ticket"))
			Expect(byTool).To(HaveKey("web/fetch"))
			Expect(byTool).To(HaveKey("costs/query"))
		})

		g.It("decides external calls by the table: pass before both legs, the byte cap and approvals after", func() {
			counts := map[string]int{}
			sent := map[string]int{} // run: external-low bytes after both legs, the oracle of the cap
			for _, c := range calls {
				if c.class != sinkclass.ExternalLow && c.class != sinkclass.ExternalHigh {
					continue
				}
				Expect(c.verdict.Match).To(BeNil(), "%s: a benign payload matched private text", where(c))
				switch {
				case !c.legs.Both():
					Expect(c.verdict.Action).To(Equal(Pass), "%s: %s %s", where(c), c.verdict.Reason, c.verdict.Detail)
					counts[string(c.class)+" before"]++
				case c.class == sinkclass.ExternalHigh:
					Expect(c.verdict.Action).To(Equal(NeedsDecision), where(c))
					Expect(c.verdict.Reasons).To(Equal([]string{ReasonBothLegs}), where(c))
					counts["external-high both"]++
				case sent[c.line.Run]+c.bytes <= DefaultByteCap:
					Expect(c.verdict.Action).To(Equal(Pass), "%s: %s %s", where(c), c.verdict.Reason, c.verdict.Detail)
					sent[c.line.Run] += c.bytes
					counts["external-low under cap"]++
				default:
					Expect(c.verdict.Action).To(Equal(Refuse), where(c))
					Expect(c.verdict.Reason).To(Equal(ReasonByteCap), where(c))
					Expect(c.verdict.Detail).To(ContainSubstring("may send at most 2048 bytes"))
					counts["external-low over cap"]++
				}
			}
			// Every branch of the table is in the corpus.
			for _, k := range []string{"external-low before", "external-high before", "external-high both", "external-low under cap", "external-low over cap"} {
				Expect(counts[k]).To(BeNumerically(">", 0), k)
			}
			g.GinkgoWriter.Printf("benign external calls by branch: %v\n", counts)
		})

		g.It("raises no false alarm on the 218 fetches out and the 272 writes of #265's shape (0 of 218, 0 of 272)", func() {
			// The counts of the decision comment on #265, kept by gen_corpus.py in runs 01 to 16. A false
			// alarm is a decision the payload or the address caused (the text match, the URL rule, an
			// unreadable address); the legs and the byte cap decide by design (ADR 0091).
			writes := map[string]bool{
				"slack/post_message": true, "mail/send_email": true, "tickets/create_ticket": true,
				"gitea/create_issue": true, "gitea/create_issue_comment": true, "gitea/create_pull_request": true,
			}
			var fetches, sent int
			for _, c := range calls {
				if c.line.Run == "run-17" || strings.HasPrefix(string(c.line.Args), `{"channel":`) && strings.Contains(string(c.line.Args), "Starting the daily check") {
					continue // the long research run and the runs' opening post are not of #265's sample
				}
				tool := c.line.Server + "/" + c.line.Tool
				switch {
				case tool == "web/fetch" && c.class == sinkclass.ExternalLow:
					fetches++
				case writes[tool]:
					sent++
				default:
					continue
				}
				Expect(c.verdict.Match).To(BeNil(), "%s: a false alarm of the text match", where(c))
				Expect(c.verdict.Reason).NotTo(BeElementOf(ReasonTaint, ReasonURLRule, ReasonAmbiguous, ReasonCheck), "%s: %s", where(c), c.verdict.Detail)
				Expect(c.verdict.Reasons).NotTo(ContainElement(ReasonTaint), where(c))
			}
			Expect(fetches).To(Equal(218))
			Expect(sent).To(Equal(272))
		})
	})

	g.Describe("planted leaks", func() {
		g.It("catches every planted leak to an external sink, and lets the internal write through", func(ctx g.SpecContext) {
			calls := mustReplay(ctx, mustCorpus(leakCorpus), replayOptions{})
			shapes, expects, caughtAt := map[string]bool{}, map[string]int{}, map[string]int{}
			for _, c := range calls {
				if c.line.Expect == "" {
					continue // the quiet fetch that gives a case its untrusted leg
				}
				expects[c.line.Expect]++
				_, shape, _ := strings.Cut(c.line.Case, "/")
				shapes[shape] = true
				v := c.verdict
				if strings.HasPrefix(c.line.Expect, "caught") {
					caughtAt[string(c.class)+" "+c.line.Server+"/"+c.line.Tool]++
				}
				switch c.line.Expect {
				case "pass":
					Expect(c.class).To(Equal(sinkclass.Internal), c.line.Case)
					Expect(v.Action).To(Equal(Pass), c.line.Case)
				case "caught":
					Expect(v.Action).NotTo(Equal(Pass), "%s (%s): %s", c.line.Case, c.line.Generation, where(c))
					if c.class == sinkclass.ExternalHigh {
						Expect(v.Action).To(Equal(NeedsDecision), c.line.Case)
						Expect(v.Reasons).To(ContainElement(ReasonBothLegs), c.line.Case)
					}
				case "caught_before_both_legs":
					Expect(c.legs.Both()).To(BeFalse(), c.line.Generation)
					Expect(v.Action).NotTo(Equal(Pass), "%s (%s): %s", c.line.Case, c.line.Generation, where(c))
					if v.Action == Refuse {
						Expect(v.Reason).To(Equal(ReasonURLRule), c.line.Case)
					} else {
						Expect(v.Reasons).To(Equal([]string{ReasonTaint}), c.line.Case)
						Expect(v.Match).NotTo(BeNil(), c.line.Case)
					}
				case "not_promised":
					// Out of call order the text match does not join (ADR 0091 decision 6); the both-legs
					// replay of the same case asserts it is caught there.
				default:
					g.Fail("unknown expect " + c.line.Expect)
				}
			}
			g.GinkgoWriter.Printf("planted leak calls by expectation: %v, caught by sink class and tool: %v\n", expects, caughtAt)
			// Per sink class: every external sink of the corpus has caught leaks, a fetch, a search, the
			// chat, the mail and a write to the org's public repository.
			for _, k := range []string{
				"external-low web/fetch", "external-low search/web_search", "external-high slack/post_message",
				"external-high mail/send_email", "external-high gitea/create_issue_comment",
			} {
				Expect(caughtAt[k]).To(BeNumerically(">", 0), "no caught leak at %s: %v", k, caughtAt)
			}
			Expect(expects["caught"]).To(BeNumerically(">=", 140))
			Expect(expects["caught_before_both_legs"]).To(BeNumerically(">=", 120))
			for _, want := range []string{
				"query-plain", "path", "percent-all", "percent-double", "base64", "base64url", "base64-in-search",
				"base64-of-base64", "base32", "hex", "hex-colon", "hex-escapes", "unicode-escapes", "html-entities",
				"utf16-base64", "uppercase", "fullwidth", "homoglyph", "zero-width", "whitespace", "dns-labels",
				"search-plain", "public-repo", "internal-write", "split-args", "split-args-b64-halves",
				"split-args-reversed", "split-calls", "whole-b64",
			} {
				Expect(shapes).To(HaveKey(want), "the leak corpus lacks the %q shape", want)
			}
			// The RT1 canaries are among the planted leaks.
			cases := map[string]bool{}
			for _, c := range calls {
				cases[strings.SplitN(c.line.Case, "/", 2)[0]] = true
			}
			Expect(cases).To(HaveKey("canary-passphrase"))
			Expect(cases).To(HaveKey("canary-bucket"))
			Expect(cases).To(HaveKey("canary-line-3"))
		})
	})

	g.It("lets a Gitea write into the org's own repo through as internal after a private read (the #352 false alarm)", func(ctx g.SpecContext) {
		// #352: owner northwind and repo platform-handbook, glued by the pair joins, matched a private
		// title and blocked 91 of 272 benign writes. Under ADR 0091 the address decides the class and is
		// never matched, and an internal write passes whatever its body carries.
		finance, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "rt1", "docs-repo", "finance-close-notes.md"))
		Expect(err).NotTo(HaveOccurred())
		header := slices.DeleteFunc(mustCorpus(benignCorpus), func(l corpusLine) bool { return l.Type == "call" || l.Type == "feed" })
		ev := func(l corpusLine) corpusLine { l.Generation, l.Run = "gen-regression", "run-regression"; return l }
		write := func(repo, body string) corpusLine {
			args, err := json.Marshal(map[string]any{"owner": "northwind", "repo": repo, "index": 73, "body": body})
			Expect(err).NotTo(HaveOccurred())
			return ev(corpusLine{Type: "call", Server: "gitea", Tool: "create_issue_comment", Args: args})
		}
		body := "Findings from this run: the written policy asks for monthly upgrades but the lockfiles show 3 months of drift. " +
			"Quoted from the finance page: " + string(finance)
		lines := append(header,
			ev(corpusLine{Type: "feed", Tool: "fs_read", Legs: corpusLegs{PrivateData: true}, Texts: []string{string(finance)}}),
			write("platform-handbook", body), // private leg only
			ev(corpusLine{
				Type: "call", Server: "web", Tool: "fetch", Args: json.RawMessage(`{"url":"https://kubernetes.io/docs/"}`),
				Result: json.RawMessage(`{"content":[{"type":"text","text":"Fake page someone outside wrote."}]}`),
			}),
			write("platform-handbook", body), // both legs
			write("public-mirror", body),     // the same write to the org's public repository
		)
		calls := mustReplay(ctx, lines, replayOptions{})
		Expect(calls).To(HaveLen(4))
		for _, c := range []replayed{calls[0], calls[2]} {
			Expect(c.class).To(Equal(sinkclass.Internal))
			Expect(c.verdict.Action).To(Equal(Pass))
			Expect(c.verdict.Match).To(BeNil(), "an internal write is never matched")
		}
		Expect(calls[0].legs.PrivateData).To(BeTrue())
		Expect(calls[2].legs.Both()).To(BeTrue())
		public := calls[3]
		Expect(public.class).To(Equal(sinkclass.ExternalHigh))
		Expect(public.verdict.Action).To(Equal(NeedsDecision))
		Expect(public.verdict.Reasons).To(ConsistOf(ReasonBothLegs, ReasonTaint))
	})

	g.It("measures the hand-built corpus: harness self-test, not a measurement", func(ctx g.SpecContext) {
		m, err := measure(ctx, mustCorpus(benignCorpus))
		Expect(err).NotTo(HaveOccurred())
		g.AddReportEntry("leak guard harness self-test", m.report("HARNESS SELF-TEST on the hand-built corpus, NOT A MEASUREMENT"), g.ReportEntryVisibilityAlways)
		// What the generator built, so the harness is seen to count it: 17 runs in 9 generations, the
		// long research run over the cap, and no text match at the default threshold.
		Expect(m.Runs).To(Equal(17))
		Expect(m.Generations).To(Equal(9))
		Expect(m.MultiRunGenerations).To(Equal(8))
		Expect(m.RunsBothLegs).To(BeNumerically(">", 0))
		Expect(m.ApprovalCalls).To(BeNumerically(">", 0))
		Expect(m.RunsOverCap).To(BeNumerically(">=", 1))
		Expect(m.Matches[24]).To(BeZero())
		Expect(m.External).To(Equal(m.Classes[sinkclass.ExternalLow] + m.Classes[sinkclass.ExternalHigh]))
		for i := 1; i < len(measureThresholds); i++ {
			Expect(m.Matches[measureThresholds[i]]).To(BeNumerically("<=", m.Matches[measureThresholds[i-1]]),
				"a higher threshold never matches more")
		}
	})

	if path := os.Getenv(corpusEnv); path != "" {
		g.It("measures the corpus named by "+corpusEnv, func(ctx g.SpecContext) {
			m, err := measure(ctx, mustCorpus(path))
			Expect(err).NotTo(HaveOccurred())
			g.AddReportEntry("leak guard measurement", m.report(path), g.ReportEntryVisibilityAlways)
		})
	}
})
