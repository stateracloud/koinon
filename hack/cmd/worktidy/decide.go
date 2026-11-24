// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Action is what the tool does, or would do, with one object.
type Action string

const (
	// ActKeep leaves the object alone.
	ActKeep Action = "keep"
	// ActRemove removes the object (a clean, merged worktree, a merged branch, unused Docker objects).
	ActRemove Action = "remove"
	// ActPatchRemove saves a patch of the uncommitted work, then removes the worktree.
	ActPatchRemove Action = "patch-remove"
	// ActReport names a problem that this tool does not fix (a broken worktree pointer, an old claim).
	ActReport Action = "report"
)

// PRState is what GitHub says about the work of a branch.
type PRState string

const (
	PRMerged PRState = "merged" // a merged pull request whose head is the local tip
	// PRMergedOther is a merged pull request, but the local branch has commits that it did not carry.
	PRMergedOther PRState = "merged-other"
	PRClosed      PRState = "closed"
	PROpen        PRState = "open"
	PRNone        PRState = "none"
	// PRUnknown means gh or the network failed: the tool decides nothing from it.
	PRUnknown PRState = "unknown"
)

// PR is one pull request, as `gh pr list` reports it.
type PR struct {
	Number  int
	State   string // OPEN, MERGED or CLOSED
	Branch  string
	HeadOID string
}

// Decision is the verdict on one object.
type Decision struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Action  Action `json:"action"`
	Reason  string `json:"reason"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
	Patch   string `json:"patch,omitempty"`
	// Detail: branch, pull request, dirty and unpushed counts of a worktree.
	Detail string `json:"detail,omitempty"`
}

// Worktree is one entry of `git worktree list`.
type Worktree struct {
	Path     string
	Head     string
	Branch   string // empty when detached
	Main     bool   // the first entry: the main checkout
	Locked   bool
	Broken   bool // the directory or its .git pointer is gone
	Current  bool // the directory the tool runs in
	Managed  bool // lives under the worktree root of hack/wt.sh
	Dirty    int  // files that git status reports (staged, unstaged, untracked)
	DirtyErr bool // git status failed
	Unpushed int  // commits on no remote branch
	UnpErr   bool
}

// Name is the name that hack/wt.sh knows the worktree by.
func (w Worktree) Name() string { return filepath.Base(w.Path) }

// prState picks the state of the work at tip from the pull requests of a branch. known is false when
// the pull request list could not be read.
func prState(prs []PR, tip string, known bool) (PRState, int) {
	if !known {
		return PRUnknown, 0
	}
	state, num := PRNone, 0
	rank := map[PRState]int{PRNone: 0, PRClosed: 1, PRMergedOther: 2, PRMerged: 3, PROpen: 4}
	for _, p := range prs {
		var s PRState
		switch p.State {
		case "OPEN":
			s = PROpen
		case "MERGED":
			s = PRMergedOther
			if p.HeadOID == tip {
				s = PRMerged
			}
		case "CLOSED":
			s = PRClosed
		default:
			continue
		}
		if rank[s] > rank[state] || (rank[s] == rank[state] && p.Number > num) {
			state, num = s, p.Number
		}
	}
	return state, num
}

func prText(s PRState, n int) string {
	if n > 0 {
		return fmt.Sprintf("%s #%d", s, n)
	}
	return string(s)
}

// decideWorktree is the rule for one worktree. Only a worktree whose work is merged and whose tree is
// clean is removed. A dirty one is removed only with allowDirty, after a patch. claimed is true when
// the identity holds a claim on the issue of the branch.
func decideWorktree(w Worktree, prs []PR, prsKnown, claimed, allowDirty bool) Decision {
	d := Decision{Kind: "worktree", Name: w.Name(), Path: w.Path, Action: ActKeep}
	state, num := PRNone, 0
	if w.Branch != "" {
		state, num = prState(prs, w.Head, prsKnown)
	}
	d.Detail = fmt.Sprintf("branch=%s pr=%s dirty=%s unpushed=%s", orDash(w.Branch), prText(state, num), countText(w.Dirty, w.DirtyErr), countText(w.Unpushed, w.UnpErr))
	switch {
	case w.Main:
		d.Reason = "main checkout"
	case !w.Managed:
		d.Reason = "outside the worktree root of hack/wt.sh"
	case w.Broken:
		d.Action, d.Reason = ActReport, "broken worktree pointer; run: git worktree prune"
	case w.Current:
		d.Reason = "the tool runs in this worktree"
	case w.Locked:
		d.Reason = "locked"
	case w.Branch == "":
		d.Reason = "detached head: no branch to prove the work is merged"
	case state == PROpen:
		d.Reason = "open pull request"
	case state == PRUnknown:
		d.Reason = "unknown: gh or the network failed, so the pull request state is not known"
	case state == PRNone:
		d.Reason = "no pull request" + unpushedText(w)
	case state == PRClosed:
		d.Reason = "pull request closed without a merge" + unpushedText(w)
	case state == PRMergedOther && (w.UnpErr || w.Unpushed > 0):
		d.Reason = "pull request merged, but the branch has commits it did not carry" + unpushedText(w)
	case claimed:
		d.Reason = "a claim on the issue is still held"
	case w.DirtyErr:
		d.Reason = "unknown: git status failed"
	case w.Dirty > 0 && !allowDirty:
		d.Reason = fmt.Sprintf("merged, but %d uncommitted file(s); use --force-dirty to save a patch and remove", w.Dirty)
	case w.Dirty > 0:
		d.Action, d.Reason = ActPatchRemove, fmt.Sprintf("merged; %d uncommitted file(s) go into a patch first", w.Dirty)
	default:
		d.Action, d.Reason = ActRemove, "merged and clean"
	}
	return d
}

func unpushedText(w Worktree) string {
	if w.UnpErr {
		return "; unpushed commits unknown"
	}
	if w.Unpushed > 0 {
		return fmt.Sprintf("; %d commit(s) on no remote branch", w.Unpushed)
	}
	return ""
}

func countText(n int, bad bool) string {
	if bad {
		return "?"
	}
	return strconv.Itoa(n)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Branch is one local branch.
type Branch struct {
	Name string
	Tip  string
}

// decideBranch is the rule for one local branch. checkedOut maps a branch to the worktree that holds
// it. A branch counts as merged only when its pull request is MERGED and carried this tip: not from
// `git branch --merged` alone, because a fresh branch is an ancestor of main too.
func decideBranch(b Branch, checkedOut map[string]string, prs []PR, prsKnown bool, protected map[string]bool) Decision {
	d := Decision{Kind: "branch", Name: b.Name, Action: ActKeep}
	state, num := prState(prs, b.Tip, prsKnown)
	d.Detail = "pr=" + prText(state, num)
	switch {
	case protected[b.Name]:
		d.Reason = "protected branch"
	case checkedOut[b.Name] != "":
		d.Reason = "checked out in " + checkedOut[b.Name]
	case state == PROpen:
		d.Reason = "open pull request"
	case state == PRUnknown:
		d.Reason = "unknown: gh or the network failed, so the pull request state is not known"
	case state == PRMerged:
		d.Action, d.Reason = ActRemove, "pull request merged with this tip"
	case state == PRMergedOther:
		d.Reason = "pull request merged, but the branch has other commits"
	case state == PRClosed:
		d.Reason = "pull request closed without a merge"
	default:
		d.Reason = "no pull request"
	}
	return d
}

// Claim is one held claim of this identity (`hack/claim.sh mine`).
type Claim struct {
	Issue int
	Lane  string
	PR    int // 0 when none
}

var branchIssue = regexp.MustCompile(`^[a-z]+/([0-9]+)-`)

// issueOfBranch returns the issue number of a branch named <type>/<N>-<slug>, or 0.
func issueOfBranch(branch string) int {
	m := branchIssue.FindStringSubmatch(branch)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// decideClaim reports a claim that has no worktree and no open pull request. It never releases.
func decideClaim(c Claim, worktreeIssues, openPRIssues map[int]bool, openPRs map[int]bool, prsKnown bool) Decision {
	d := Decision{Kind: "claim", Name: "#" + strconv.Itoa(c.Issue), Action: ActKeep, Detail: "lane=" + orDash(c.Lane)}
	switch {
	case worktreeIssues[c.Issue]:
		d.Reason = "a worktree exists"
	case !prsKnown:
		d.Reason = "unknown: gh or the network failed, so the pull request state is not known"
	case openPRIssues[c.Issue] || (c.PR > 0 && openPRs[c.PR]):
		d.Reason = "an open pull request exists"
	default:
		d.Action = ActReport
		d.Reason = fmt.Sprintf("no worktree and no open pull request; release with: hack/claim.sh release %d", c.Issue)
	}
	return d
}

// DockerFacts is what `docker` says. Err is set when docker cannot be reached.
type DockerFacts struct {
	Err        string
	Running    []string // names of running containers
	Kind       []string // names of kind cluster containers (running or not)
	Stopped    int
	UnusedImgs int
	UnusedSize string
}

// decideDocker is the rule for the opt-in Docker clean-up. It never touches volumes.
func decideDocker(f DockerFacts, force bool) []Decision {
	mk := func(name string, a Action, reason string) Decision {
		return Decision{Kind: "docker", Name: name, Action: a, Reason: reason}
	}
	if f.Err != "" {
		return []Decision{mk("docker", ActKeep, "unknown: "+f.Err)}
	}
	var block []string
	if len(f.Running) > 0 {
		block = append(block, fmt.Sprintf("%d running container(s): %s", len(f.Running), joinMax(f.Running, 3)))
	}
	if len(f.Kind) > 0 {
		block = append(block, fmt.Sprintf("%d kind node container(s): %s", len(f.Kind), joinMax(f.Kind, 3)))
	}
	if len(block) > 0 && !force {
		return []Decision{mk("docker", ActKeep, "refused while "+strings.Join(block, " and ")+"; use --force-docker")}
	}
	c := mk("stopped-containers", ActRemove, fmt.Sprintf("%d stopped container(s)", f.Stopped))
	i := mk("unused-images", ActRemove, fmt.Sprintf("about %d unused image(s) (estimate: docker image prune --all removes every image that no container uses), %s reclaimable", f.UnusedImgs, orDash(f.UnusedSize)))
	if f.Stopped == 0 {
		c.Action, c.Reason = ActKeep, "no stopped container"
	}
	if f.UnusedImgs == 0 {
		i.Action, i.Reason = ActKeep, "no unused image"
	}
	return []Decision{c, i}
}

func joinMax(s []string, n int) string {
	s = append([]string(nil), s...)
	sort.Strings(s)
	if len(s) > n {
		return strings.Join(s[:n], ", ") + ", ..."
	}
	return strings.Join(s, ", ")
}

// isManaged says whether a worktree path is one that hack/wt.sh manages (is_managed in wt.sh): under
// the worktree root; or, when KOINON_WT_ROOT is not set (rootSet false), directly inside a directory
// named kw whose parent is a temp base: /tmp, /private/tmp, /private/tmp/claude-*, /var/folders/*/*/T
// (also under /private) or one of bases ($TMPDIR and KOINON_WT_TMP_BASES).
func isManaged(path, root string, rootSet bool, bases []string) bool {
	path = filepath.Clean(path)
	if root != "" {
		root = filepath.Clean(root)
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	if rootSet {
		return false
	}
	parent := filepath.Dir(path)
	if filepath.Base(parent) != "kw" {
		return false
	}
	grand := filepath.Dir(parent)
	for _, pat := range []string{"/tmp", "/private/tmp", "/private/tmp/claude-*", "/var/folders/*/*/T", "/private/var/folders/*/*/T"} {
		if ok, _ := filepath.Match(pat, grand); ok {
			return true
		}
	}
	for _, b := range bases {
		if b != "" && filepath.Clean(b) == grand {
			return true
		}
	}
	return false
}
