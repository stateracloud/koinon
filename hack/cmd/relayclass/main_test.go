// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRelayclass(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "relayclass")
}

const head = "0123456789abcdef0123456789abcdef01234567"

type fakeGH struct {
	pr       PR
	comments []Comment
	runs     []CheckRun
	err      error
	calls    []string
}

func (f *fakeGH) PR(repo string, _ int) (PR, error) {
	f.calls = append(f.calls, "pr "+repo)
	return f.pr, f.err
}

func (f *fakeGH) Comments(string, int) ([]Comment, error) { return f.comments, f.err }

func (f *fakeGH) Comment(repo string, id int64) (Comment, error) {
	f.calls = append(f.calls, "comment "+repo)
	for _, c := range f.comments {
		if c.ID == id {
			return c, nil
		}
	}
	return Comment{}, errors.New("not found")
}

func (f *fakeGH) CheckRuns(string, string) ([]CheckRun, error) { return f.runs, f.err }

func status(id int64, at time.Time, lines ...string) Comment {
	return Comment{ID: id, CreatedAt: at, Body: StatusMarker + "\n" + strings.Join(lines, "\n")}
}

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func required(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

var _ = Describe("commentVerdict", func() {
	DescribeTable("one comment",
		func(body, want, wantReady string) {
			v, r := commentVerdict(body)
			Expect(v).To(Equal(want))
			Expect(r).To(Equal(wantReady))
		},
		Entry("no marker: a bot or a human", "review: READY at "+head+"\nSTOP", VerdictNothing, ""),
		Entry("progress only", StatusMarker+"\nTests pass. Review runs.", VerdictNothing, ""),
		Entry("READY", StatusMarker+"\nreview: READY at "+head, VerdictNothing, head),
		Entry("STOP wins over all", StatusMarker+"\nquestion: which?\nreview: READY at "+head+"\n**STOP**: round 2 has a major finding", VerdictStop, head),
		Entry("question", StatusMarker+"\n- Question: may I change the API?", VerdictQuestion, ""),
		Entry("needs main", StatusMarker+"\nneeds main: the .github PR #12", VerdictQuestion, ""),
		Entry("a word inside a line is not a verdict", StatusMarker+"\nI do not STOP here. No question remains.", VerdictNothing, ""),
		Entry("lower-case stop is not STOP", StatusMarker+"\nstop the old run first", VerdictNothing, ""),
		Entry("a quoted question line is not news", StatusMarker+"\n> question: which?\n> **STOP**\n> review: READY at "+head, VerdictNothing, ""),
		Entry("a quoted line does not hide an unquoted one", StatusMarker+"\n> question: old\nquestion: new", VerdictQuestion, ""),
		Entry("Question: none is not a question", StatusMarker+"\nQuestion: none.\n**Needs main:** n/a\nquestions:", VerdictNothing, ""),
	)
})

var _ = Describe("ClassifyPR", func() {
	open := PR{Number: 7, State: "open", HeadSHA: head}
	req := required("go", "quick checks")

	It("says nothing new for an open pull request with green checks and no status", func() {
		res := ClassifyPR(open, []Comment{{ID: 1, Body: "LGTM", CreatedAt: t0}},
			[]CheckRun{{ID: 1, Name: "go", Status: "completed", Conclusion: "success"}}, req, time.Time{})
		Expect(res.Verdict).To(Equal(VerdictNothing))
	})

	It("reports READY at the head", func() {
		res := ClassifyPR(open, []Comment{status(1, t0, "review: READY at "+head[:12])}, nil, req, time.Time{})
		Expect(res.Verdict).To(Equal("READY at " + head[:12]))
	})

	It("ignores a stale READY at an older commit", func() {
		res := ClassifyPR(open, []Comment{status(1, t0, "review: READY at 1111111111111111")}, nil, req, time.Time{})
		Expect(res.Verdict).To(Equal(VerdictNothing))
		Expect(res.Why).To(ContainSubstring("stale READY"))
	})

	It("reads only the newest status comment without --since", func() {
		cs := []Comment{
			status(2, t0.Add(time.Minute), "Tests run."),
			status(1, t0, "question: which?"),
		}
		Expect(ClassifyPR(open, cs, nil, req, time.Time{}).Verdict).To(Equal(VerdictNothing))
	})

	It("reads every comment after --since and keeps the most urgent", func() {
		cs := []Comment{
			status(1, t0, "STOP: old"),
			status(2, t0.Add(2*time.Minute), "review: READY at "+head),
			status(3, t0.Add(3*time.Minute), "question: new"),
		}
		res := ClassifyPR(open, cs, nil, req, t0.Add(time.Minute))
		Expect(res.Verdict).To(Equal(VerdictQuestion))
		Expect(res.Why).To(Equal("comment 3"))
	})

	It("reports a red required check over READY, and judges the newest run of a name", func() {
		runs := []CheckRun{
			{ID: 1, Name: "go", Status: "completed", Conclusion: "failure"},
			{ID: 2, Name: "go", Status: "completed", Conclusion: "success"},
			{ID: 3, Name: "quick checks", Status: "completed", Conclusion: "timed_out"},
			{ID: 4, Name: "fuzz", Status: "completed", Conclusion: "failure"},
		}
		res := ClassifyPR(open, []Comment{status(1, t0, "review: READY at "+head)}, runs, req, time.Time{})
		Expect(res.Verdict).To(Equal(VerdictRed))
		Expect(res.Why).To(HaveSuffix(": quick checks"))
	})

	It("does not call a running or a re-run check red", func() {
		runs := []CheckRun{
			{ID: 1, Name: "go", Status: "completed", Conclusion: "failure"},
			{ID: 2, Name: "go", Status: "in_progress"},
		}
		Expect(ClassifyPR(open, nil, runs, req, time.Time{}).Verdict).To(Equal(VerdictNothing))
	})

	It("reports a draft below READY and above nothing new", func() {
		d := open
		d.Draft = true
		Expect(ClassifyPR(d, nil, nil, req, time.Time{}).Verdict).To(Equal(VerdictDraft))
		Expect(ClassifyPR(d, []Comment{status(1, t0, "review: READY at "+head)}, nil, req, time.Time{}).Verdict).To(HavePrefix(VerdictReady))
	})

	It("reads READY in the pull request body, also for a draft, but not for an older head", func() {
		d := open
		d.Draft = true
		d.Body = "## Summary\n\nreview: READY at " + head
		res := ClassifyPR(d, nil, nil, req, time.Time{})
		Expect(res.Verdict).To(Equal("READY at " + head))
		Expect(res.Why).To(Equal("pull request 7 body"))
		d.Body = "review: READY at 1111111111111111"
		Expect(ClassifyPR(d, nil, nil, req, time.Time{}).Verdict).To(Equal(VerdictDraft))
	})

	It("says nothing new for a closed or merged pull request", func() {
		m := open
		m.State, m.Merged = "closed", true
		Expect(ClassifyPR(m, []Comment{status(1, t0, "STOP")}, nil, req, time.Time{}).Verdict).To(Equal(VerdictNothing))
	})
})

var _ = Describe("ParseRequired", func() {
	It("reads the names of the real list and skips the advisory ones", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "ci-required.txt"))
		Expect(err).NotTo(HaveOccurred())
		req := ParseRequired(string(raw))
		Expect(req).To(HaveKey("go"))
		Expect(req).To(HaveKey("quick checks"))
		Expect(req).To(HaveKey("e2e result"))
		Expect(req).NotTo(HaveKey("fuzz"))
		Expect(req).NotTo(HaveKey("advisory fuzz"))
	})
})

var _ = Describe("run", func() {
	var (
		gh             *fakeGH
		root           string
		stdout, stderr *bytes.Buffer
	)
	BeforeEach(func() {
		root = GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(root, "hack"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "hack", "ci-required.txt"), []byte("aggregator go\nadvisory fuzz\n"), 0o600)).To(Succeed())
		gh = &fakeGH{pr: PR{Number: 7, State: "open", HeadSHA: head}}
		stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	})

	It("prints one line for a pull request number", func() {
		gh.runs = []CheckRun{{ID: 1, Name: "go", Status: "completed", Conclusion: "failure"}}
		Expect(run([]string{"7"}, gh, root, stdout, stderr)).To(Equal(0))
		Expect(stdout.String()).To(Equal("red required check\n"))
		Expect(stderr.String()).To(ContainSubstring("red on 0123456789ab: go"))
	})

	It("classifies only the comment of a comment URL, against the head of its pull request", func() {
		gh.comments = []Comment{status(42, t0, "review: READY at "+head), status(43, t0, "STOP")}
		Expect(run([]string{"https://github.com/o/r/pull/7#issuecomment-42"}, gh, root, stdout, stderr)).To(Equal(0))
		Expect(stdout.String()).To(Equal("READY at " + head + "\n"))
		Expect(gh.calls).To(ConsistOf("comment o/r", "pr o/r"))
	})

	It("says nothing new for a comment URL of a merged pull request", func() {
		gh.pr.State, gh.pr.Merged = "closed", true
		gh.comments = []Comment{status(42, t0, "STOP")}
		Expect(run([]string{"https://github.com/o/r/pull/7#issuecomment-42"}, gh, root, stdout, stderr)).To(Equal(0))
		Expect(stdout.String()).To(Equal("nothing new\n"))
	})

	It("does not read a pull request for a comment on an issue", func() {
		gh.comments = []Comment{status(42, t0, "question: scope?")}
		Expect(run([]string{"https://github.com/o/r/issues/9#issuecomment-42"}, gh, root, stdout, stderr)).To(Equal(0))
		Expect(stdout.String()).To(Equal("question\n"))
		Expect(gh.calls).To(ConsistOf("comment o/r"))
	})

	DescribeTable("usage and read errors exit 2 and print no verdict",
		func(args []string, setup func(*fakeGH)) {
			if setup != nil {
				setup(gh)
			}
			Expect(run(args, gh, root, stdout, stderr)).To(Equal(2))
			Expect(stdout.String()).To(BeEmpty())
		},
		Entry("no argument", []string{}, nil),
		Entry("two arguments", []string{"1", "2"}, nil),
		Entry("not a number or a URL", []string{"https://github.com/o/r/pull/7"}, nil),
		Entry("a bad --since", []string{"--since", "yesterday", "7"}, nil),
		Entry("a GitHub error", []string{"7"}, func(f *fakeGH) { f.err = errors.New("HTTP 502") }),
		Entry("no required list", []string{"--required", "/nonexistent", "7"}, nil),
	)
})

var _ = Describe("pages", func() {
	It("decodes the pages of gh api --paginate", func() {
		ps, err := pages[[]apiComment]([]byte(`[{"id":1}][{"id":2},{"id":3}]`))
		Expect(err).NotTo(HaveOccurred())
		Expect(ps).To(HaveLen(2))
		Expect(ps[1]).To(HaveLen(2))
	})

	It("fails on broken JSON", func() {
		_, err := pages[[]apiComment]([]byte(`[{"id":1}`))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ghClient", func() {
	It("reads a pull request, its comments and its check runs through the gh command", func() {
		dir := GinkgoT().TempDir()
		fake := filepath.Join(dir, "gh")
		script := `#!/bin/sh
case "$*" in
"api repos/o/r/pulls/7") echo '{"number":7,"state":"open","draft":true,"body":"b","head":{"sha":"` + head + `"}}' ;;
"api --paginate repos/o/r/issues/7/comments?per_page=100") echo '[{"id":1,"body":"a","created_at":"2026-11-27T12:00:00Z"}]'; echo '[{"id":2,"body":"b","created_at":"2026-11-27T12:01:00Z"}]' ;;
"api --paginate repos/o/r/commits/` + head + `/check-runs?per_page=100") echo '{"total_count":1,"check_runs":[{"id":5,"name":"go","status":"completed","conclusion":"failure"}]}' ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(fake, []byte(script), 0o700)).To(Succeed())
		g := &ghClient{bin: fake}
		pr, err := g.PR("o/r", 7)
		Expect(err).NotTo(HaveOccurred())
		Expect(pr).To(Equal(PR{Number: 7, State: "open", Draft: true, HeadSHA: head, Body: "b"}))
		cs, err := g.Comments("o/r", 7)
		Expect(err).NotTo(HaveOccurred())
		Expect(cs).To(HaveLen(2))
		Expect(cs[1].CreatedAt).To(Equal(t0.Add(time.Minute)))
		runs, err := g.CheckRuns("o/r", head)
		Expect(err).NotTo(HaveOccurred())
		Expect(runs).To(Equal([]CheckRun{{ID: 5, Name: "go", Status: "completed", Conclusion: "failure"}}))
		_, err = g.Comment("o/r", 9)
		Expect(err).To(MatchError(ContainSubstring("unexpected")))
	})
})
