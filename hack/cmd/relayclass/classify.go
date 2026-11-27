// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The verdicts, from the most urgent to the least. Exactly one is printed.
const (
	VerdictStop     = "STOP"
	VerdictQuestion = "question"
	VerdictRed      = "red required check"
	VerdictReady    = "READY at " // + the sha
	VerdictDraft    = "draft"
	VerdictNothing  = "nothing new"
)

// StatusMarker starts every lane status comment (docs/maintainers/hooks.md, "The relay classifier").
// Comments without it (bots, humans, CI) never make news.
const StatusMarker = "<!-- koinon-lane-status -->"

// Comment is one issue comment of a pull request.
type Comment struct {
	ID        int64
	Body      string
	CreatedAt time.Time
}

// CheckRun is one check run on the head commit.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, ...
}

// PR is the part of a pull request that the classifier reads.
type PR struct {
	Number  int
	State   string // open, closed
	Merged  bool
	Draft   bool
	HeadSHA string
	Body    string // a lane may also put `review: READY at <sha>` in the body
}

var (
	readyRe    = regexp.MustCompile(`review:\s*READY at ([0-9a-f]{7,40})\b`)
	stopRe     = regexp.MustCompile(`^[^A-Za-z0-9]*STOP\b`)
	questionRe = regexp.MustCompile(`(?i)^[^a-z0-9]*(?:question|needs main)\b[\s:*_-]*(.*)$`)
)

// noQuestion are the rests of a question line that say there is none ("Question: none").
var noQuestion = map[string]bool{"": true, "none": true, "n/a": true, "no": true, "nothing": true, "-": true}

// isQuestion says whether a line asks main something: `question: ...` or `needs main ...` with a rest
// that is not empty and not "none".
func isQuestion(line string) bool {
	m := questionRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	return !noQuestion[strings.ToLower(strings.Trim(m[1], " .*_"))]
}

// commentVerdict classifies the body of one comment. ready is the sha of a READY line, if any.
// It returns VerdictNothing for a comment without the status marker.
func commentVerdict(body string) (verdict, ready string) {
	if !strings.HasPrefix(strings.TrimSpace(body), StatusMarker) {
		return VerdictNothing, ""
	}
	verdict = VerdictNothing
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// A quoted line (`> question: ...`) repeats an older text. It is not news.
		if strings.HasPrefix(line, ">") {
			continue
		}
		switch {
		case stopRe.MatchString(line):
			return VerdictStop, ready
		case isQuestion(line):
			verdict = VerdictQuestion
		default:
			if m := readyRe.FindStringSubmatch(line); m != nil && ready == "" {
				ready = m[1]
			}
		}
	}
	return verdict, ready
}

// rank orders the verdicts; a lower rank wins.
func rank(v string) int {
	switch {
	case v == VerdictStop:
		return 0
	case v == VerdictQuestion:
		return 1
	case v == VerdictRed:
		return 2
	case strings.HasPrefix(v, VerdictReady):
		return 3
	case v == VerdictDraft:
		return 4
	}
	return 5
}

// redConclusions are the conclusions of a completed check run that block a merge.
var redConclusions = map[string]bool{
	"failure": true, "timed_out": true, "cancelled": true, "action_required": true, "startup_failure": true, "stale": true,
}

// redRequired returns the required names whose newest check run (the highest id) completed red.
// A check that is queued or still running is not red.
func redRequired(runs []CheckRun, required map[string]bool) []string {
	newest := map[string]CheckRun{}
	for _, r := range runs {
		if !required[r.Name] {
			continue
		}
		if cur, ok := newest[r.Name]; !ok || r.ID > cur.ID {
			newest[r.Name] = r
		}
	}
	var red []string
	for name, r := range newest {
		if r.Status == "completed" && redConclusions[r.Conclusion] {
			red = append(red, name)
		}
	}
	sort.Strings(red)
	return red
}

// sameCommit says whether a (possibly short) sha names the head.
func sameCommit(sha, head string) bool {
	return len(sha) >= 7 && head != "" && strings.HasPrefix(head, sha)
}

// Result is the verdict and one line that says why, for stderr.
type Result struct {
	Verdict string
	Why     string
}

func better(cur, next Result) Result {
	if rank(next.Verdict) < rank(cur.Verdict) {
		return next
	}
	return cur
}

// ClassifyComments reduces status comments to one verdict. head is the head sha of the pull
// request ("" when unknown); a READY for another commit is stale and says nothing new.
func ClassifyComments(comments []Comment, head string) Result {
	res := Result{Verdict: VerdictNothing, Why: "no status comment with news"}
	for _, c := range comments {
		v, ready := commentVerdict(c.Body)
		if v == VerdictNothing && ready != "" {
			if head != "" && !sameCommit(ready, head) {
				if res.Verdict == VerdictNothing {
					res.Why = fmt.Sprintf("comment %d: stale READY at %s (head %s)", c.ID, ready, short(head))
				}
				continue
			}
			v = VerdictReady + ready
		}
		res = better(res, Result{v, fmt.Sprintf("comment %d", c.ID)})
	}
	return res
}

// ClassifyPR gives the verdict of a pull request from its state, its body, its head checks and the
// status comments created after since (when since is zero, only the newest status comment counts).
func ClassifyPR(pr PR, comments []Comment, runs []CheckRun, required map[string]bool, since time.Time) Result {
	if r, done := closed(pr); done {
		return r
	}
	var picked []Comment
	if since.IsZero() {
		var last *Comment
		for i := range comments {
			if strings.HasPrefix(strings.TrimSpace(comments[i].Body), StatusMarker) &&
				(last == nil || comments[i].CreatedAt.After(last.CreatedAt)) {
				last = &comments[i]
			}
		}
		if last != nil {
			picked = []Comment{*last}
		}
	} else {
		for _, c := range comments {
			if c.CreatedAt.After(since) {
				picked = append(picked, c)
			}
		}
	}
	res := ClassifyComments(picked, pr.HeadSHA)
	if m := readyRe.FindStringSubmatch(pr.Body); m != nil && sameCommit(m[1], pr.HeadSHA) {
		res = better(res, Result{VerdictReady + m[1], fmt.Sprintf("pull request %d body", pr.Number)})
	}
	if red := redRequired(runs, required); len(red) > 0 {
		res = better(res, Result{VerdictRed, "red on " + short(pr.HeadSHA) + ": " + strings.Join(red, ", ")})
	}
	if pr.Draft {
		res = better(res, Result{VerdictDraft, fmt.Sprintf("pull request %d is a draft", pr.Number)})
	}
	return res
}

// closed gives nothing new for a closed or merged pull request: no news can change it.
func closed(pr PR) (Result, bool) {
	switch {
	case pr.Merged:
		return Result{VerdictNothing, fmt.Sprintf("pull request %d is merged", pr.Number)}, true
	case pr.State != "open":
		return Result{VerdictNothing, fmt.Sprintf("pull request %d is %s", pr.Number, pr.State)}, true
	}
	return Result{}, false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ParseRequired reads hack/ci-required.txt: one check name per line, `#` comments, an `aggregator ` or
// `external ` prefix dropped, `advisory` lines skipped (the rule of hack/ci-merge-group-test.sh).
func ParseRequired(s string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		first, rest, _ := strings.Cut(line, " ")
		switch first {
		case "advisory":
			continue
		case "aggregator", "external":
			line = strings.TrimSpace(rest)
		}
		out[line] = true
	}
	return out
}
