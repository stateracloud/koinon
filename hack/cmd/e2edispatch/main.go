// SPDX-License-Identifier: Apache-2.0

// Command e2edispatch starts one run of .github/workflows/e2e-kind.yml, finds that run and prints
// `run=<id> url=<url>` (issue 1488). It replaces the chain `gh workflow run ... && sleep && gh run list`,
// which the sleep hook refuses and which can pick the wrong run.
//
//	e2edispatch --ref REF [--suite go|web|drill] [--backup-restore] [--stories]
//	            [--commit SHA --bisect-id ID] [--timeout 2m] [--dry-run]
//
// It finds the run in this order: the run URL that `gh workflow run` prints (newer gh and GitHub
// return it); else, with --bisect-id, the dispatched run whose name carries the commit and the bisect
// id, not there before the dispatch; else the new workflow_dispatch run on the ref that was not there before the dispatch. When two
// runs match (another dispatch on the same ref at the same time), it cannot know which one is its
// own: it prints nothing on stdout, lists them on stderr and exits 1.
// Exit 0: found. 1: no single run showed up in time. 2: usage error, or a gh call failed (ADR 0118).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const workflow = "e2e-kind.yml"

// Runner runs the gh command and returns its stdout and stderr.
type Runner interface {
	Gh(args ...string) (stdout, stderr []byte, err error)
}

type execRunner struct{ bin string }

func (r execRunner) Gh(args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, args...) //nolint:gosec // the gh command; arguments are built here
	cmd.Env = append(os.Environ(), "GH_PAGER=cat", "PAGER=cat", "GH_PROMPT_DISABLED=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// Clock lets the tests run the wait without waiting.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

type options struct {
	ref, suite, commit, bisectID string
	backupRestore, stories, dry  bool
	timeout, poll                time.Duration
}

// Run is one workflow run as `gh run list --json` prints it.
type Run struct {
	ID        int64     `json:"databaseId"`
	URL       string    `json:"url"`
	Title     string    `json:"displayTitle"`
	CreatedAt time.Time `json:"createdAt"`
}

var runURLRe = regexp.MustCompile(`https://\S+/actions/runs/(\d+)`)

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("e2edispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.ref, "ref", "", "the branch or tag to run the workflow on (required)")
	fs.StringVar(&o.suite, "suite", "go", "go, web or drill")
	fs.BoolVar(&o.backupRestore, "backup-restore", false, "also run the backup-and-restore step (go suite only)")
	fs.BoolVar(&o.stories, "stories", false, "also run the story suite (go suite only)")
	fs.StringVar(&o.commit, "commit", "", "run on this full commit sha instead of the ref (hack/e2e-bisect.sh)")
	fs.StringVar(&o.bisectID, "bisect-id", "", "the bisect run id that the run name carries (needs --commit)")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "how long to look for the new run")
	fs.DurationVar(&o.poll, "poll", 5*time.Second, "the time between two looks")
	fs.BoolVar(&o.dry, "dry-run", false, "print the gh command and do not dispatch")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fs.NArg() != 0:
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case o.ref == "":
		return o, errors.New("--ref is required")
	case o.suite != "go" && o.suite != "web" && o.suite != "drill":
		return o, fmt.Errorf("--suite must be go, web or drill, not %q", o.suite)
	case (o.backupRestore || o.stories) && o.suite != "go":
		return o, errors.New("--backup-restore and --stories need --suite go")
	case o.bisectID != "" && o.commit == "":
		return o, errors.New("--bisect-id needs --commit")
	case o.timeout <= 0 || o.poll <= 0:
		return o, errors.New("--timeout and --poll must be positive")
	}
	return o, nil
}

func dispatchArgs(o options) []string {
	args := []string{"workflow", "run", workflow, "--ref", o.ref, "-f", "suite=" + o.suite}
	if o.backupRestore {
		args = append(args, "-f", "backup_restore=true")
	}
	if o.stories {
		args = append(args, "-f", "stories=true")
	}
	if o.commit != "" {
		args = append(args, "-f", "commit="+o.commit)
	}
	if o.bisectID != "" {
		args = append(args, "-f", "bisect_id="+o.bisectID)
	}
	return args
}

// branch is the name that `gh run list --branch` matches for a ref.
func branch(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
}

func listRuns(gh Runner, ref string) ([]Run, error) {
	out, errOut, err := gh.Gh("run", "list", "--workflow", workflow, "--event", "workflow_dispatch", "--branch", branch(ref),
		"--limit", "50", "--json", "databaseId,url,displayTitle,createdAt")
	if err != nil {
		return nil, fmt.Errorf("gh run list: %w: %s", err, strings.TrimSpace(string(errOut)))
	}
	var runs []Run
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("gh run list: %w", err)
	}
	return runs, nil
}

// matches returns the matching runs, the oldest first.
func matches(runs []Run, match func(Run) bool) []Run {
	var m []Run
	for _, r := range runs {
		if match(r) {
			m = append(m, r)
		}
	}
	sort.Slice(m, func(i, j int) bool { return m[i].ID < m[j].ID })
	return m
}

func run(args []string, gh Runner, clk Clock, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintf(stderr, "e2edispatch: %v\n", err)
		}
		return 2
	}
	dargs := dispatchArgs(o)
	if o.dry {
		_, _ = fmt.Fprintf(stdout, "would dispatch: gh %s\n", strings.Join(dargs, " "))
		return 0
	}
	// A run that exists before the dispatch is never the new run. With a bisect id this also keeps out
	// a run of an earlier attempt that carries the same name.
	before := map[int64]bool{}
	runs, err := listRuns(gh, o.ref)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "e2edispatch: %v\n", err)
		return 2
	}
	for _, r := range runs {
		before[r.ID] = true
	}
	start := clk.Now()
	out, errOut, err := gh.Gh(dargs...)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "e2edispatch: gh %s: %v: %s\n", strings.Join(dargs, " "), err, strings.TrimSpace(string(errOut)))
		return 2
	}
	if m := runURLRe.FindSubmatch(append(out, errOut...)); m != nil {
		_, _ = fmt.Fprintf(stdout, "run=%s url=%s\n", m[1], m[0])
		return 0
	}
	match := func(r Run) bool { return !before[r.ID] }
	if o.bisectID != "" {
		match = func(r Run) bool {
			return !before[r.ID] && strings.Contains(r.Title, o.commit) && strings.Contains(r.Title, "bisect "+o.bisectID)
		}
	}
	for {
		runs, err := listRuns(gh, o.ref)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "e2edispatch: %v\n", err)
			return 2
		}
		switch m := matches(runs, match); len(m) {
		case 0:
		case 1:
			_, _ = fmt.Fprintf(stdout, "run=%d url=%s\n", m[0].ID, m[0].URL)
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "e2edispatch: %d new runs on %s (another dispatch ran at the same time); find yours by its inputs:\n", len(m), o.ref)
			for _, r := range m {
				_, _ = fmt.Fprintf(stderr, "  run=%d url=%s\n", r.ID, r.URL)
			}
			return 1
		}
		left := o.timeout - clk.Now().Sub(start)
		if left <= 0 {
			_, _ = fmt.Fprintf(stderr, "e2edispatch: dispatched on %s, but no new run showed up within %s; look with gh run list --workflow %s --branch %s\n",
				o.ref, o.timeout, workflow, branch(o.ref))
			return 1
		}
		clk.Sleep(min(o.poll, left))
	}
}

func main() {
	bin := os.Getenv("GH")
	if bin == "" {
		bin = "gh"
	}
	os.Exit(run(os.Args[1:], execRunner{bin: bin}, realClock{}, os.Stdout, os.Stderr))
}
