// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2edispatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "e2edispatch")
}

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// fakeGh answers `run list` from lists (one per call, the last one repeats) and `workflow run` with
// dispatchOut.
type fakeGh struct {
	lists       [][]Run
	listCalls   int
	dispatchOut string
	dispatchErr error
	calls       []string
}

func (f *fakeGh) Gh(args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] + " " + args[1] {
	case "workflow run":
		return []byte(f.dispatchOut), []byte("koinon_test_stderr"), f.dispatchErr
	case "run list":
		i := min(f.listCalls, len(f.lists)-1)
		f.listCalls++
		var l []Run
		if i >= 0 {
			l = f.lists[i]
		}
		out, err := json.Marshal(l)
		return out, nil, err
	}
	return nil, nil, errors.New("unexpected gh " + strings.Join(args, " "))
}

func url(id string) string { return "https://github.com/o/r/actions/runs/" + id }

var _ = Describe("e2edispatch", func() {
	var (
		gh             *fakeGh
		clk            *fakeClock
		stdout, stderr *bytes.Buffer
	)
	BeforeEach(func() {
		gh = &fakeGh{}
		clk = &fakeClock{now: t0}
		stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	})
	do := func(args ...string) int { return run(args, gh, clk, stdout, stderr) }

	It("takes the run URL that gh workflow run prints", func() {
		gh.lists = [][]Run{{{ID: 5, CreatedAt: t0.Add(-time.Hour)}}}
		gh.dispatchOut = "✓ Created workflow_dispatch event for e2e-kind.yml at main\n" + url("777") + "\n"
		Expect(do("--ref", "main", "--suite", "web")).To(Equal(0))
		Expect(stdout.String()).To(Equal("run=777 url=" + url("777") + "\n"))
		Expect(gh.calls).To(ContainElement("workflow run e2e-kind.yml --ref main -f suite=web"))
	})

	It("passes every input", func() {
		gh.dispatchOut = url("1")
		Expect(do("--ref", "main", "--backup-restore", "--stories", "--commit", "abc", "--bisect-id", "99")).To(Equal(0))
		Expect(gh.calls).To(ConsistOf(
			"workflow run e2e-kind.yml --ref main -f suite=go -f backup_restore=true -f stories=true -f commit=abc -f bisect_id=99"))
	})

	It("finds the new run on the ref when gh prints no URL, and waits for it", func() {
		old := Run{ID: 5, URL: url("5"), CreatedAt: t0.Add(-time.Hour)}
		fresh := Run{ID: 9, URL: url("9"), CreatedAt: t0.Add(3 * time.Second)}
		gh.lists = [][]Run{{old}, {old}, {fresh, old}}
		Expect(do("--ref", "refs/tags/v0.1.0-rc.1", "--suite", "drill")).To(Equal(0))
		Expect(stdout.String()).To(Equal("run=9 url=" + url("9") + "\n"))
		Expect(gh.calls[0]).To(ContainSubstring("--branch v0.1.0-rc.1"))
		Expect(clk.now).To(Equal(t0.Add(5 * time.Second)))
	})

	It("does not take a run that was there before, even if it is recent", func() {
		recent := Run{ID: 8, URL: url("8"), CreatedAt: t0}
		gh.lists = [][]Run{{recent}}
		Expect(do("--ref", "main", "--timeout", "20s")).To(Equal(1))
		Expect(stdout.String()).To(BeEmpty())
		Expect(stderr.String()).To(ContainSubstring("no new run showed up within 20s"))
	})

	It("prints the oldest of two new runs and warns", func() {
		gh.lists = [][]Run{{}, {{ID: 12, URL: url("12"), CreatedAt: t0}, {ID: 11, URL: url("11"), CreatedAt: t0}}}
		Expect(do("--ref", "main")).To(Equal(0))
		Expect(stdout.String()).To(Equal("run=11 url=" + url("11") + "\n"))
		Expect(stderr.String()).To(ContainSubstring("2 new runs"))
	})

	It("matches the run of a bisect by its name and does not list before the dispatch", func() {
		gh.lists = [][]Run{{
			{ID: 111, URL: url("111"), Title: "e2e-kind (abc) bisect 555"},
			{ID: 222, URL: url("222"), Title: "e2e-kind (abc) bisect 999"},
		}}
		Expect(do("--ref", "main", "--commit", "abc", "--bisect-id", "999")).To(Equal(0))
		Expect(stdout.String()).To(Equal("run=222 url=" + url("222") + "\n"))
		Expect(gh.calls[0]).To(HavePrefix("workflow run"))
	})

	It("exits 1 when the dispatch fails", func() {
		gh.dispatchErr = errors.New("exit status 1")
		Expect(do("--ref", "main")).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("koinon_test_stderr"))
	})

	It("prints the command and calls nothing on --dry-run", func() {
		Expect(do("--ref", "main", "--commit", "abc", "--bisect-id", "7", "--backup-restore", "--dry-run")).To(Equal(0))
		Expect(stdout.String()).To(Equal("would dispatch: gh workflow run e2e-kind.yml --ref main -f suite=go -f backup_restore=true -f commit=abc -f bisect_id=7\n"))
		Expect(gh.calls).To(BeEmpty())
	})

	DescribeTable("usage errors exit 2 and call nothing",
		func(args ...string) {
			Expect(do(args...)).To(Equal(2))
			Expect(gh.calls).To(BeEmpty())
			Expect(stdout.String()).To(BeEmpty())
		},
		Entry("no ref"),
		Entry("an unknown suite", "--ref", "main", "--suite", "lab"),
		Entry("backup-restore outside the go suite", "--ref", "main", "--suite", "web", "--backup-restore"),
		Entry("stories outside the go suite", "--ref", "main", "--suite", "drill", "--stories"),
		Entry("a bisect id without a commit", "--ref", "main", "--bisect-id", "1"),
		Entry("an argument", "--ref", "main", "extra"),
		Entry("an unknown flag", "--ref", "main", "--wait"),
	)
})
