// SPDX-License-Identifier: Apache-2.0

// Command worktidy reports, and on request cleans, what a maintainer who runs many lanes collects:
// finished worktrees, merged local branches, old claims and (opt-in) unused Docker objects (issue
// 1231). `make tidy` runs it as a dry run; `make tidy APPLY=1` does the work.
//
// The rules (decide.go) are pure functions. Git, gh, claim.sh and docker sit behind the small
// interfaces below, so the tests use fakes and need no network and no Docker.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Git is what the tool needs from git and from hack/wt.sh.
type Git interface {
	Worktrees() ([]Worktree, error)
	Status(path string) (dirty int, err error)
	Unpushed(path string) (int, error)
	Branches() ([]Branch, error)
	// SavePatch writes the staged, unstaged and untracked changes of a worktree under dir and
	// returns the directory that holds them.
	SavePatch(w Worktree, dir string) (string, error)
	// RemoveClean removes a clean worktree through hack/wt.sh rm, which checks claims and processes.
	RemoveClean(w Worktree) error
	// RemoveForce removes a worktree whose changes are saved.
	RemoveForce(w Worktree) error
	DeleteBranch(name string) error
}

// Forge lists pull requests by head branch. An error means unknown.
type Forge interface {
	PRs() (map[string][]PR, error)
}

// Claims lists the claims this identity holds. An error means unknown.
type Claims interface {
	Mine() ([]Claim, error)
}

// Docker is the opt-in part.
type Docker interface {
	Facts() DockerFacts
	PruneContainers() error
	PruneImages() error
}

// Deps are the outside world of one run.
type Deps struct {
	Git    Git
	Forge  Forge
	Claims Claims
	Docker Docker
	Cwd    string
	WTRoot string
	// WTRootSet is true when KOINON_WT_ROOT is set; then only WTRoot is managed.
	WTRootSet bool
	// TmpBases are extra temp bases ($TMPDIR, KOINON_WT_TMP_BASES) for the kw parent rule.
	TmpBases []string
}

// Options are the flags.
type Options struct {
	Apply       bool
	JSON        bool
	ForceDirty  bool
	WithDocker  bool
	ForceDocker bool
	PatchDir    string
}

// plan reads the facts and decides every object. It changes nothing.
func plan(d Deps, o Options) ([]Decision, error) {
	wts, err := d.Git.Worktrees()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	prsByBranch, prErr := d.Forge.PRs()
	prsKnown := prErr == nil
	openIssues := map[int]bool{}
	openPRs := map[int]bool{}
	for br, list := range prsByBranch {
		for _, p := range list {
			if p.State == "OPEN" {
				openPRs[p.Number] = true
				if n := issueOfBranch(br); n > 0 {
					openIssues[n] = true
				}
			}
		}
	}
	claims, claimErr := d.Claims.Mine()
	claimed := map[int]bool{}
	for _, c := range claims {
		claimed[c.Issue] = true
	}

	var out []Decision
	checkedOut := map[string]string{}
	wtIssues := map[int]bool{}
	for i := range wts {
		w := &wts[i]
		w.Managed = isManaged(w.Path, d.WTRoot, d.WTRootSet, d.TmpBases)
		w.Current = samePath(w.Path, d.Cwd)
		if !w.Main && !w.Broken {
			if n, err := d.Git.Status(w.Path); err != nil {
				w.DirtyErr = true
			} else {
				w.Dirty = n
			}
			if n, err := d.Git.Unpushed(w.Path); err != nil {
				w.UnpErr = true
			} else {
				w.Unpushed = n
			}
		}
		if n := issueOfBranch(w.Branch); n > 0 {
			wtIssues[n] = true
		}
		dec := decideWorktree(*w, prsByBranch[w.Branch], prsKnown, claimed[issueOfBranch(w.Branch)], o.ForceDirty)
		out = append(out, dec)
		// A worktree that goes away frees its branch for the branch pass.
		if w.Branch != "" && dec.Action != ActRemove && dec.Action != ActPatchRemove {
			checkedOut[w.Branch] = w.Path
		}
	}

	branches, err := d.Git.Branches()
	if err != nil {
		return nil, fmt.Errorf("git branch: %w", err)
	}
	protected := map[string]bool{"main": true, "master": true}
	for _, b := range branches {
		out = append(out, decideBranch(b, checkedOut, prsByBranch[b.Name], prsKnown, protected))
	}

	if claimErr == nil {
		for _, c := range claims {
			out = append(out, decideClaim(c, wtIssues, openIssues, openPRs, prsKnown))
		}
	} else {
		out = append(out, Decision{Kind: "claim", Name: "claims", Action: ActKeep, Reason: "unknown: " + claimErr.Error()})
	}

	if o.WithDocker {
		out = append(out, decideDocker(d.Docker.Facts(), o.ForceDocker)...)
	}
	return out, nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// apply does what the decisions say, in order: worktrees, branches, Docker. A failure is recorded on
// its decision and the next object still runs.
func apply(d Deps, o Options, decs []Decision, wts map[string]Worktree) {
	stuck := map[string]bool{} // branches whose worktree could not be removed
	for i := range decs {
		dec := &decs[i]
		var err error
		switch {
		case dec.Action != ActRemove && dec.Action != ActPatchRemove:
			continue
		case dec.Kind == "worktree":
			w := wts[dec.Path]
			if dec.Action == ActPatchRemove {
				dec.Patch, err = d.Git.SavePatch(w, o.PatchDir)
				if err == nil {
					err = d.Git.RemoveForce(w)
				}
			} else {
				err = d.Git.RemoveClean(w)
			}
		case dec.Kind == "branch" && stuck[dec.Name]:
			dec.Action, dec.Reason = ActKeep, "its worktree could not be removed"
			continue
		case dec.Kind == "branch":
			err = d.Git.DeleteBranch(dec.Name)
		case dec.Kind == "docker" && dec.Name == "stopped-containers":
			err = d.Docker.PruneContainers()
		case dec.Kind == "docker" && dec.Name == "unused-images":
			err = d.Docker.PruneImages()
		}
		if err != nil {
			dec.Error = err.Error()
			if dec.Kind == "worktree" {
				stuck[wts[dec.Path].Branch] = true
			}
		} else {
			dec.Applied = true
		}
	}
}

// run is the whole program; it returns the exit code (0 done, 1 an object failed, 2 usage or read error).
func run(d Deps, o Options, stdout, stderr io.Writer) int {
	decs, err := plan(d, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "worktidy:", err)
		return 2
	}
	if o.Apply {
		wts, err := d.Git.Worktrees()
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "worktidy:", err)
			return 2
		}
		byPath := map[string]Worktree{}
		for _, w := range wts {
			byPath[w.Path] = w
		}
		apply(d, o, decs, byPath)
	}
	sort.SliceStable(decs, func(i, j int) bool { return kindOrder(decs[i].Kind) < kindOrder(decs[j].Kind) })
	code := 0
	for _, dec := range decs {
		if dec.Error != "" {
			code = 1
		}
	}
	if o.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(decs); err != nil {
			_, _ = fmt.Fprintln(stderr, "worktidy:", err)
			return 2
		}
		return code
	}
	for _, dec := range decs {
		_, _ = fmt.Fprintln(stdout, line(dec, o.Apply))
	}
	if !o.Apply {
		_, _ = fmt.Fprintln(stdout, "dry run: nothing changed; run with --apply to do it")
	}
	return code
}

func kindOrder(k string) int {
	switch k {
	case "worktree":
		return 0
	case "branch":
		return 1
	case "claim":
		return 2
	}
	return 3
}

// line is the one-line text of a decision.
func line(d Decision, applied bool) string {
	verb := string(d.Action)
	switch {
	case d.Error != "":
		verb = "failed"
	case d.Action == ActRemove || d.Action == ActPatchRemove:
		verb = "would-" + verb
		if applied && d.Applied {
			verb = "removed"
			if d.Action == ActPatchRemove {
				verb = "patched-removed"
			}
		}
	}
	s := fmt.Sprintf("%s %s %s: %s", d.Kind, d.Name, verb, d.Reason)
	if d.Detail != "" {
		s += " [" + d.Detail + "]"
	}
	if d.Patch != "" {
		s += " patch=" + d.Patch
	}
	if d.Error != "" {
		s += " error=" + d.Error
	}
	return s
}

func main() {
	var o Options
	flag.BoolVar(&o.Apply, "apply", false, "do the clean-up (the default is a dry run)")
	flag.BoolVar(&o.JSON, "json", false, "print one JSON array instead of one line for each object")
	flag.BoolVar(&o.ForceDirty, "force-dirty", false, "also remove a merged worktree with uncommitted files, after saving a patch")
	flag.BoolVar(&o.WithDocker, "docker", false, "also clean stopped containers and unused images (never volumes)")
	flag.BoolVar(&o.ForceDocker, "force-docker", false, "with --docker: go on while a container runs or a kind cluster exists")
	flag.StringVar(&o.PatchDir, "patch-dir", "", "where --force-dirty saves patches (default: <worktree root>/.patches)")
	flag.Parse()

	root := os.Getenv("KOINON_WT_ROOT")
	rootSet := root != ""
	if root == "" {
		root = "/tmp/kw"
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	if o.PatchDir == "" {
		o.PatchDir = filepath.Join(root, ".patches")
	}
	cwd, _ := os.Getwd()
	g := newExecGit(cwd)
	d := Deps{
		Git:       g,
		Forge:     execForge{dir: cwd},
		Claims:    execClaims{dir: g.toplevel},
		Docker:    execDocker{},
		Cwd:       g.toplevel,
		WTRoot:    root,
		WTRootSet: rootSet,
		TmpBases:  tmpBases(),
	}
	os.Exit(run(d, o, os.Stdout, os.Stderr))
}

// tmpBases lists the extra temp bases wt.sh accepts: $TMPDIR and KOINON_WT_TMP_BASES, resolved.
func tmpBases() []string {
	var r []string
	cands := append([]string{os.Getenv("TMPDIR")}, strings.Split(os.Getenv("KOINON_WT_TMP_BASES"), ":")...)
	for _, b := range cands {
		if b == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(b); err == nil {
			b = real
		}
		r = append(r, filepath.Clean(b))
	}
	return r
}
