// SPDX-License-Identifier: Apache-2.0

// Command relayclass says in one line whether a GitHub event of a pull request is news for the main
// session (issue 1488). The GitHub-to-Slack relay hook calls it before it starts a model turn, so an
// event that says nothing new costs no turn (docs/maintainers/hooks.md, "The relay classifier").
//
//	relayclass [--repo OWNER/REPO] [--since RFC3339] [--required FILE] <PR number | comment URL>
//
// It prints exactly one of: `STOP`, `question`, `red required check`, `READY at <sha>`, `draft`,
// `nothing new` on stdout, and one line that says why on stderr. Exit 0 with a verdict, 2 for a usage
// or read error (ADR 0118).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// GitHub is what the classifier reads. ghClient implements it with `gh api`.
type GitHub interface {
	PR(repo string, n int) (PR, error)
	Comments(repo string, n int) ([]Comment, error)
	Comment(repo string, id int64) (Comment, error)
	CheckRuns(repo, sha string) ([]CheckRun, error)
}

// commentURLRe matches the html_url of an issue comment: .../OWNER/REPO/(pull|issues)/N#issuecomment-ID.
var commentURLRe = regexp.MustCompile(`^https://[^/]+/([^/]+/[^/]+)/(pull|issues)/(\d+)#issuecomment-(\d+)$`)

type target struct {
	repo    string
	number  int
	comment int64 // 0: the whole pull request
	isPR    bool  // false for a comment on an issue
}

func parseTarget(arg, repo string) (target, error) {
	if n, err := strconv.Atoi(arg); err == nil && n > 0 {
		return target{repo: repo, number: n, isPR: true}, nil
	}
	m := commentURLRe.FindStringSubmatch(arg)
	if m == nil {
		return target{}, fmt.Errorf("want a pull request number or a comment URL (https://github.com/OWNER/REPO/pull/N#issuecomment-ID), got %q", arg)
	}
	n, _ := strconv.Atoi(m[3])
	id, _ := strconv.ParseInt(m[4], 10, 64)
	return target{repo: m[1], number: n, comment: id, isPR: m[2] == "pull"}, nil
}

func run(args []string, gh GitHub, repoRoot string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("relayclass", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "{owner}/{repo}", "the repository (gh fills {owner}/{repo} from GH_REPO or the git remote)")
	sinceS := fs.String("since", "", "pull request mode: only status comments created after this RFC3339 time count (default: the newest one)")
	reqFile := fs.String("required", filepath.Join(repoRoot, "hack", "ci-required.txt"), "the list of required checks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: relayclass [--repo OWNER/REPO] [--since RFC3339] [--required FILE] <PR number | comment URL>")
		return 2
	}
	var since time.Time
	if *sinceS != "" {
		t, err := time.Parse(time.RFC3339, *sinceS)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "relayclass: --since: %v\n", err)
			return 2
		}
		since = t
	}
	tg, err := parseTarget(fs.Arg(0), *repo)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "relayclass: %v\n", err)
		return 2
	}
	res, err := classify(tg, gh, *reqFile, since)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "relayclass: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintln(stdout, res.Verdict)
	_, _ = fmt.Fprintf(stderr, "relayclass: %s\n", res.Why)
	return 0
}

func classify(tg target, gh GitHub, reqFile string, since time.Time) (Result, error) {
	if tg.comment != 0 {
		c, err := gh.Comment(tg.repo, tg.comment)
		if err != nil {
			return Result{}, err
		}
		head := ""
		if tg.isPR {
			pr, err := gh.PR(tg.repo, tg.number)
			if err != nil {
				return Result{}, err
			}
			if r, done := closed(pr); done {
				return r, nil
			}
			head = pr.HeadSHA
		}
		return ClassifyComments([]Comment{c}, head), nil
	}
	raw, err := os.ReadFile(reqFile) //nolint:gosec // a path from the flag or the repository
	if err != nil {
		return Result{}, fmt.Errorf("read the required checks: %w", err)
	}
	required := ParseRequired(string(raw))
	if len(required) == 0 {
		return Result{}, errors.New(reqFile + " lists no required check")
	}
	pr, err := gh.PR(tg.repo, tg.number)
	if err != nil {
		return Result{}, err
	}
	comments, err := gh.Comments(tg.repo, tg.number)
	if err != nil {
		return Result{}, err
	}
	runs, err := gh.CheckRuns(tg.repo, pr.HeadSHA)
	if err != nil {
		return Result{}, err
	}
	return ClassifyPR(pr, comments, runs, required, since), nil
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayclass: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(os.Args[1:], newGHClient(), root, os.Stdout, os.Stderr))
}
