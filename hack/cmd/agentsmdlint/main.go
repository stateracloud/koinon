// SPDX-License-Identifier: Apache-2.0

// Command agentsmdlint keeps the agent instruction files in step with the tree (issue 1045,
// docs/agents/agents-md.md). Checks:
//
//   - no CLAUDE.md anywhere: Claude Code reads AGENTS.md natively, and a CLAUDE.md would hide the
//     nested files;
//   - the root AGENTS.md has an index line (a backticked `<folder>/AGENTS.md`) for every nested file
//     and for nothing else;
//   - budgets: the root has at most 150 lines (target 110), a nested file at most 120 (target 60),
//     and no file is above 10 KB. A line count above a target only prints a note;
//   - the first line of each nested file names the root file and its own folder;
//   - every `make <target>`, `hack/` script and other backticked repo path named in an AGENTS.md, and
//     every relative Markdown link in it, exists;
//   - no rule appears in two files: a sentence of 30 or more characters that two files share.
//
// Scope: the files git tracks, or every file when ROOT is not a git checkout (as
// hack/old-issue-refs-lint.sh does).
//
// Usage: agentsmdlint ROOT. hack/agentsmd-lint.sh is the wrapper that passes the repository root.
// Exit codes: 0 clean, 1 findings, 2 usage or read error (ADR 0118).
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	rootTarget   = 110
	rootCeiling  = 150
	nestedTarget = 60
	nestedCeil   = 120
	maxBytes     = 10 * 1024
	minRuleChars = 30
	agentsName   = "AGENTS.md"
)

var (
	spanRe     = regexp.MustCompile("`([^`\n]+)`")
	linkRe     = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	makeRe     = regexp.MustCompile(`(?:^|\s)make\s+([a-z][a-z0-9_.-]*)`)
	targetRe   = regexp.MustCompile(`(?m)^([a-zA-Z0-9][a-zA-Z0-9_.-]*)\s*:`)
	pathRe     = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`)
	sentenceRe = regexp.MustCompile(`[.!?;]\s+`)
	spaceRe    = regexp.MustCompile(`\s+`)
	bulletRe   = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s+`)
)

type finding struct{ file, msg string }

func (f finding) String() string { return f.file + ": " + f.msg }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: agentsmdlint ROOT")
		os.Exit(2)
	}
	code, err := run(os.Args[1], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentsmdlint:", err)
	}
	os.Exit(code)
}

// run lints the tree under root; it returns the exit code.
func run(root string, stdout, stderr io.Writer) (int, error) {
	files, err := listFiles(root)
	if err != nil {
		return 2, err
	}
	found, notes, err := lint(os.DirFS(root), files)
	if err != nil {
		return 2, err
	}
	for _, n := range notes {
		_, _ = fmt.Fprintln(stdout, "note:", n)
	}
	if len(found) > 0 {
		_, _ = fmt.Fprintln(stderr, "FAIL: the agent instruction files break the rules of docs/agents/agents-md.md:")
		for _, f := range found {
			_, _ = fmt.Fprintln(stderr, "  "+f.String())
		}
		return 1, nil
	}
	_, _ = fmt.Fprintln(stdout, "agents-md ok")
	return 0, nil
}

// listFiles returns the tracked files (slash paths relative to root) of a git checkout, or every
// file of a plain directory.
func listFiles(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil { //nolint:gosec // ROOT is the operator argument; the git arguments are fixed
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	ctx := context.Background()
	top, err := exec.CommandContext(ctx, "git", "-C", abs, "rev-parse", "--show-toplevel").Output() //nolint:gosec // ROOT is the operator argument; the git arguments are fixed
	if err == nil {
		if real, err := filepath.EvalSymlinks(strings.TrimSpace(string(top))); err == nil && real == abs {
			out, err := exec.CommandContext(ctx, "git", "-C", abs, "ls-files", "-z").Output() //nolint:gosec // ROOT is the operator argument; the git arguments are fixed
			if err != nil {
				return nil, fmt.Errorf("git ls-files: %w", err)
			}
			var files []string
			for _, p := range bytes.Split(out, []byte{0}) {
				if len(p) == 0 {
					continue
				}
				// A tracked file deleted in the working tree is not a file any more.
				if _, err := os.Lstat(filepath.Join(abs, string(p))); err == nil { //nolint:gosec // ROOT is the operator argument; the git arguments are fixed
					files = append(files, string(p))
				}
			}
			return files, nil
		}
	}
	var files []string
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // ROOT is the operator argument; the git arguments are fixed
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// lint checks the files of fsys; notes are budget targets that a file exceeds.
func lint(fsys fs.FS, files []string) (found []finding, notes []string, err error) {
	var agents []string
	for _, f := range files {
		base := path.Base(f)
		switch {
		case strings.EqualFold(base, "CLAUDE.md"):
			found = append(found, finding{f, "no CLAUDE.md in the tree: Claude Code reads AGENTS.md itself, and a CLAUDE.md hides the nested files"})
		case base == agentsName:
			agents = append(agents, f)
		}
	}
	sort.Strings(agents)
	if !slices.Contains(agents, agentsName) {
		found = append(found, finding{agentsName, "the root file is missing"})
		return found, notes, nil
	}

	makefile, _ := fs.ReadFile(fsys, "Makefile")
	targets := makeTargets(string(makefile))

	contents := map[string]string{}
	for _, f := range agents {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, nil, err
		}
		contents[f] = string(b)
		fl, fn := checkBudget(f, string(b))
		found = append(found, fl...)
		notes = append(notes, fn...)
		if f != agentsName {
			found = append(found, checkFirstLine(f, string(b))...)
		}
		found = append(found, checkRefs(fsys, f, string(b), targets)...)
	}
	found = append(found, checkIndex(agents, contents[agentsName])...)
	found = append(found, checkDuplicates(agents, contents)...)
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].msg < found[j].msg
	})
	return found, notes, nil
}

func checkBudget(f, text string) (found []finding, notes []string) {
	lines := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		lines++
	}
	target, ceiling := nestedTarget, nestedCeil
	if f == agentsName {
		target, ceiling = rootTarget, rootCeiling
	}
	if lines > ceiling {
		found = append(found, finding{f, fmt.Sprintf("%d lines; the ceiling is %d (move detail to a nested file or a linked doc)", lines, ceiling)})
	} else if lines > target {
		notes = append(notes, fmt.Sprintf("%s has %d lines; the target is %d", f, lines, target))
	}
	if len(text) > maxBytes {
		found = append(found, finding{f, fmt.Sprintf("%d bytes; no file may be above %d", len(text), maxBytes)})
	}
	return found, notes
}

// firstLine is the required first line of a nested file for the folder dir.
func firstLine(dir string) string {
	return "The root AGENTS.md applies too; this file adds rules for `" + dir + "/`."
}

func checkFirstLine(f, text string) []finding {
	first, _, _ := strings.Cut(text, "\n")
	if want := firstLine(path.Dir(f)); strings.TrimRight(first, " \r") != want {
		return []finding{{f, "the first line must be: " + want}}
	}
	return nil
}

// checkIndex compares the nested files with the backticked nested paths of the root file.
func checkIndex(agents []string, root string) []finding {
	have := map[string]bool{}
	for _, f := range agents {
		if f != agentsName {
			have[f] = true
		}
	}
	listed := map[string]bool{}
	for _, m := range spanRe.FindAllStringSubmatch(root, -1) {
		if p := m[1]; strings.HasSuffix(p, "/"+agentsName) {
			listed[p] = true
		}
	}
	var found []finding
	for f := range have {
		if !listed[f] {
			found = append(found, finding{agentsName, "the index has no line for `" + f + "`"})
		}
	}
	for p := range listed {
		if !have[p] {
			found = append(found, finding{agentsName, "the index names `" + p + "`, which does not exist"})
		}
	}
	return found
}

// makeTargets returns the targets a Makefile defines.
func makeTargets(makefile string) map[string]bool {
	t := map[string]bool{}
	for _, m := range targetRe.FindAllStringSubmatch(makefile, -1) {
		t[m[1]] = true
	}
	return t
}

// inProse returns the text of a Markdown file without its fenced code blocks.
func inProse(text string) string {
	var b strings.Builder
	fenced := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// checkRefs checks the make targets, repo paths and relative links that f names.
func checkRefs(fsys fs.FS, f, text string, targets map[string]bool) []finding {
	var found []finding
	seen := map[string]bool{}
	add := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			found = append(found, finding{f, msg})
		}
	}
	prose := inProse(text)
	for _, m := range spanRe.FindAllStringSubmatch(prose, -1) {
		span := m[1]
		for _, mk := range makeRe.FindAllStringSubmatch(span, -1) {
			if !targets[mk[1]] {
				add("`make " + mk[1] + "`: the Makefile has no such target")
			}
		}
		for _, tok := range strings.Fields(span) {
			p, ok := repoPath(fsys, tok)
			if !ok {
				continue
			}
			if _, err := fs.Stat(fsys, p); err != nil {
				add("`" + tok + "`: the path does not exist")
			}
		}
	}
	for _, m := range linkRe.FindAllStringSubmatch(prose, -1) {
		target, _, _ := strings.Cut(m[1], "#")
		if target == "" || strings.Contains(target, ":") || strings.HasPrefix(target, "/") {
			continue
		}
		p := path.Join(path.Dir(f), target)
		if strings.HasPrefix(p, "..") {
			add("link " + m[1] + " leaves the repository")
		} else if _, err := fs.Stat(fsys, p); err != nil {
			add("link " + m[1] + ": the file does not exist")
		}
	}
	return found
}

// repoPath reports whether a word of a code span names a repository path (its first segment is
// an entry of the root and it has a slash), and returns it cleaned. Placeholders (<x>, *, $, =)
// and absolute paths never count.
func repoPath(fsys fs.FS, tok string) (string, bool) {
	tok = strings.TrimRight(tok, ",.;:)")
	tok, _, _ = strings.Cut(tok, "#")
	if strings.ContainsAny(tok, "*<>${}=()") || !pathRe.MatchString(tok) || !strings.Contains(tok, "/") {
		return "", false
	}
	first, _, _ := strings.Cut(tok, "/")
	if _, err := fs.Stat(fsys, first); err != nil {
		return "", false
	}
	return path.Clean(tok), true
}

// checkDuplicates flags a rule sentence that two files both carry.
func checkDuplicates(agents []string, contents map[string]string) []finding {
	owner := map[string]string{}
	var found []finding
	for _, f := range agents {
		own := map[string]bool{}
		for _, rule := range rules(contents[f]) {
			for _, s := range sentenceRe.Split(rule, -1) {
				n := normalize(s)
				if len([]rune(n)) < minRuleChars || own[n] {
					continue
				}
				own[n] = true
				if o, ok := owner[n]; ok && o != f {
					found = append(found, finding{f, "rule also in " + o + ": " + excerpt(n)})
				} else if !ok {
					owner[n] = f
				}
			}
		}
	}
	return found
}

// rules returns the list items of a file, each joined with its continuation lines.
func rules(text string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, l := range strings.Split(inProse(text), "\n") {
		switch {
		case bulletRe.MatchString(l):
			flush()
			cur = []string{bulletRe.ReplaceAllString(l, "")}
		case strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#"):
			flush()
		case cur != nil:
			cur = append(cur, strings.TrimSpace(l))
		}
	}
	flush()
	return out
}

func normalize(s string) string {
	s = strings.NewReplacer("*", "", "_", "", "`", "").Replace(strings.ToLower(s))
	return strings.TrimRight(strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")), ".!?;")
}

func excerpt(s string) string {
	r := []rune(s)
	if len(r) > 70 {
		return string(r[:70]) + "..."
	}
	return s
}
