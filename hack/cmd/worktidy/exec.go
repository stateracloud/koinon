// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const cmdTimeout = 2 * time.Minute

// sh runs one command with no pager and no prompt, and returns its stdout.
func sh(dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed program names; arguments come from git and the flags of this tool
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "PAGER=cat", "GH_PAGER=cat", "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

// execGit implements Git with the git binary and hack/wt.sh.
type execGit struct {
	dir      string
	toplevel string
}

func newExecGit(cwd string) *execGit {
	g := &execGit{dir: cwd, toplevel: cwd}
	if out, err := sh(cwd, "git", "rev-parse", "--show-toplevel"); err == nil {
		g.toplevel = strings.TrimSpace(string(out))
	}
	return g
}

func (g *execGit) Worktrees() ([]Worktree, error) {
	out, err := sh(g.dir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(out)), nil
}

// parseWorktrees reads `git worktree list --porcelain`. The first entry is the main checkout.
func parseWorktrees(s string) []Worktree {
	var wts []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil {
			if _, err := os.Stat(filepath.Join(cur.Path, ".git")); err != nil {
				cur.Broken = true
			}
			wts = append(wts, *cur)
			cur = nil
		}
	}
	for _, l := range strings.Split(s, "\n") {
		key, val, _ := strings.Cut(l, " ")
		switch key {
		case "worktree":
			flush()
			cur = &Worktree{Path: val, Main: len(wts) == 0}
		case "HEAD":
			if cur != nil {
				cur.Head = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		case "prunable":
			if cur != nil {
				cur.Broken = true
			}
		}
	}
	flush()
	return wts
}

func (g *execGit) Status(path string) (int, error) {
	out, err := sh(path, "git", "status", "--porcelain=v1", "-uall")
	if err != nil {
		return 0, err
	}
	return len(nonEmptyLines(string(out))), nil
}

func (g *execGit) Unpushed(path string) (int, error) {
	out, err := sh(path, "git", "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func (g *execGit) Branches() ([]Branch, error) {
	out, err := sh(g.dir, "git", "for-each-ref", "--format=%(refname:short)%09%(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var bs []Branch
	for _, l := range nonEmptyLines(string(out)) {
		name, tip, _ := strings.Cut(l, "\t")
		bs = append(bs, Branch{Name: name, Tip: tip})
	}
	return bs, nil
}

// SavePatch writes <dir>/<name>-<time>/{staged,unstaged,untracked}.patch. Each patch applies with
// `git apply`. The untracked patch holds every untracked file that git does not ignore.
func (g *execGit) SavePatch(w Worktree, dir string) (string, error) {
	out := filepath.Join(dir, w.Name()+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(out, 0o750); err != nil {
		return "", err
	}
	staged, err := sh(w.Path, "git", "diff", "--cached", "--binary")
	if err != nil {
		return "", err
	}
	unstaged, err := sh(w.Path, "git", "diff", "--binary")
	if err != nil {
		return "", err
	}
	names, err := sh(w.Path, "git", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var untracked bytes.Buffer
	for _, f := range strings.Split(string(names), "\x00") {
		if f == "" {
			continue
		}
		// --no-index exits 1 when the files differ, which is the normal case here.
		b, err := sh(w.Path, "git", "diff", "--no-index", "--binary", "--", "/dev/null", f)
		if err != nil && len(b) == 0 {
			return "", err
		}
		untracked.Write(b)
	}
	for name, data := range map[string][]byte{"staged.patch": staged, "unstaged.patch": unstaged, "untracked.patch": untracked.Bytes()} {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o600); err != nil {
			return "", err
		}
	}
	return out, nil
}

func (g *execGit) RemoveClean(w Worktree) error {
	_, err := sh(g.toplevel, filepath.Join(g.toplevel, "hack", "wt.sh"), "rm", w.Name())
	return err
}

func (g *execGit) RemoveForce(w Worktree) error {
	// The patch is already saved. wt.sh --force skips its process check, so refuse here first.
	if pids := cwdProcs(w.Path); len(pids) > 0 {
		return fmt.Errorf("a process has its working directory inside %s (pid %s)", w.Path, strings.Join(pids, ","))
	}
	_, err := sh(g.toplevel, filepath.Join(g.toplevel, "hack", "wt.sh"), "rm", "--force", w.Name())
	return err
}

func (g *execGit) DeleteBranch(name string) error {
	_, err := sh(g.dir, "git", "branch", "-D", name)
	return err
}

func nonEmptyLines(s string) []string {
	var r []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			r = append(r, l)
		}
	}
	return r
}

// execForge reads pull requests with one gh call for each state, so 300 branches cost 3 calls.
type execForge struct{ dir string }

func (f execForge) PRs() (map[string][]PR, error) {
	res := map[string][]PR{}
	for _, state := range []string{"open", "merged", "closed"} {
		out, err := sh(f.dir, "gh", "pr", "list", "--state", state, "--limit", "3000", "--json", "number,state,headRefName,headRefOid")
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Number  int    `json:"number"`
			State   string `json:"state"`
			Branch  string `json:"headRefName"`
			HeadOID string `json:"headRefOid"`
		}
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			res[r.Branch] = append(res[r.Branch], PR{Number: r.Number, State: r.State, Branch: r.Branch, HeadOID: r.HeadOID})
		}
	}
	return res, nil
}

// execClaims reads `hack/claim.sh mine`: "#N<TAB>lane<TAB>pr P<TAB>holder".
type execClaims struct{ dir string }

func (c execClaims) Mine() ([]Claim, error) {
	out, err := sh(c.dir, filepath.Join(c.dir, "hack", "claim.sh"), "mine")
	if err != nil {
		return nil, err
	}
	return parseClaims(string(out)), nil
}

func parseClaims(s string) []Claim {
	var cs []Claim
	for _, l := range nonEmptyLines(s) {
		f := strings.Split(l, "\t")
		n, err := strconv.Atoi(strings.TrimPrefix(f[0], "#"))
		if err != nil {
			continue
		}
		cl := Claim{Issue: n}
		if len(f) > 1 {
			cl.Lane = f[1]
		}
		if len(f) > 2 {
			cl.PR, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(f[2], "pr")))
		}
		cs = append(cs, cl)
	}
	return cs
}

// execDocker implements Docker with the docker CLI. It has no code path for volumes.
type execDocker struct{}

func (execDocker) Facts() DockerFacts {
	var f DockerFacts
	out, err := sh("", "docker", "ps", "-a", "--no-trunc", "--format", "{{.Names}}\t{{.State}}\t{{.Image}}\t{{.Label \"io.x-k8s.kind.cluster\"}}")
	if err != nil {
		f.Err = "docker is not reachable: " + firstLine(err.Error())
		return f
	}
	usedByRunning := map[string]bool{}
	for _, l := range nonEmptyLines(string(out)) {
		p := strings.Split(l, "\t")
		if len(p) < 4 {
			continue
		}
		if p[3] != "" {
			f.Kind = append(f.Kind, p[0])
		}
		if p[1] == "running" || p[1] == "restarting" || p[1] == "paused" {
			f.Running = append(f.Running, p[0])
			usedByRunning[p[2]] = true
		} else {
			f.Stopped++
		}
	}
	imgs, err := sh("", "docker", "image", "ls", "-a", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}}\t{{.ID}}\t{{.Size}}")
	if err != nil {
		f.Err = "docker image ls failed: " + firstLine(err.Error())
		return f
	}
	for _, l := range nonEmptyLines(string(imgs)) {
		p := strings.Split(l, "\t")
		if len(p) < 3 {
			continue
		}
		short := strings.TrimPrefix(p[1], "sha256:")
		if usedByRunning[p[0]] || usedByRunning[p[1]] || (len(short) >= 12 && usedByRunning[short[:12]]) {
			continue
		}
		f.UnusedImgs++
	}
	return f
}

func (execDocker) PruneContainers() error {
	_, err := sh("", "docker", "container", "prune", "--force")
	return err
}

func (execDocker) PruneImages() error {
	_, err := sh("", "docker", "image", "prune", "--all", "--force")
	return err
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// cwdProcs returns the pids of other processes whose working directory is inside dir. It runs lsof from
// outside dir (lsof lists itself, and its cwd would be dir), and reads /proc where lsof is missing, as
// wt.sh proc_reason does.
func cwdProcs(dir string) []string {
	real, e := filepath.EvalSymlinks(dir)
	if e != nil {
		real = dir
	}
	self := strconv.Itoa(os.Getpid())
	if _, err := exec.LookPath("lsof"); err == nil {
		out, err := sh("", "lsof", "-w", "-d", "cwd", "-Fpcn")
		if err == nil || len(out) > 0 {
			return parseLsofCwd(string(out), dir, real, self)
		}
		return nil
	}
	var hits []string
	matches, _ := filepath.Glob("/proc/[0-9]*/cwd")
	for _, m := range matches {
		pid := strings.TrimSuffix(strings.TrimPrefix(m, "/proc/"), "/cwd")
		n, err := os.Readlink(m)
		if err == nil && pid != self && inDir(n, dir, real) {
			hits = append(hits, pid)
		}
	}
	return hits
}

func inDir(n, dir, real string) bool {
	return n == real || strings.HasPrefix(n, real+"/") || n == dir || strings.HasPrefix(n, dir+"/")
}

// parseLsofCwd reads `lsof -Fpcn` output and returns the pids whose cwd is inside dir. It skips the pid
// self and any lsof process (the child that produced the output).
func parseLsofCwd(out, dir, real, self string) []string {
	var pid, cmd string
	var hits []string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "p"):
			pid, cmd = l[1:], ""
		case strings.HasPrefix(l, "c"):
			cmd = l[1:]
		case strings.HasPrefix(l, "n") && pid != "" && pid != self && cmd != "lsof":
			if inDir(l[1:], dir, real) {
				hits = append(hits, pid)
			}
		}
	}
	return hits
}
