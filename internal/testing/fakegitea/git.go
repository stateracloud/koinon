// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// repo is the fake's state of one repository: a bare git repository on disk plus the issues,
// pull requests and comments that Gitea keeps in its database.
type repo struct {
	id      int64
	name    string
	dir     string
	created time.Time

	nextIndex int64 // issues and pull requests share one counter
	issues    map[int64]*issue
	pulls     []*pull
	nextCID   int64
	comments  []*comment
	// calls counts write calls by endpoint, e.g. "POST pulls".
	calls map[string]int
}

type issue struct {
	index   int64
	title   string
	body    string
	user    string
	created time.Time
}

type pull struct {
	issue
	head, base       string // branch names
	headSHA, baseSHA string
	state            string // open or closed
}

type comment struct {
	id      int64
	index   int64
	user    string
	body    string
	created time.Time
}

// sig is a git identity line.
type sig struct {
	Name, Email string
	When        time.Time
}

type gitCommit struct {
	SHA, Tree         string
	Parents           []string
	Author, Committer sig
	Message           string
}

// entry is one line of git ls-tree -l.
type entry struct {
	Mode, Type, SHA string
	Size            int64
	Path            string
}

var gitBaseEnv = []string{
	"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
}

// git runs git in the repository and returns stdout. Failures that mean "no such object" are
// left to callers to interpret.
func (rp *repo) git(stdin []byte, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // fixed binary; arguments are built by this package
	cmd.Env = append(append(slicesClone(gitBaseEnv), "GIT_DIR="+rp.dir), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func identityEnv(a, c sig) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + a.Name, "GIT_AUTHOR_EMAIL=" + a.Email,
		"GIT_AUTHOR_DATE=" + strconv.FormatInt(a.When.Unix(), 10) + " +0000",
		"GIT_COMMITTER_NAME=" + c.Name, "GIT_COMMITTER_EMAIL=" + c.Email,
		"GIT_COMMITTER_DATE=" + strconv.FormatInt(c.When.Unix(), 10) + " +0000",
	}
}

// initRepo creates the bare repository with a main branch holding README.md. Pushes over
// HTTP are enabled for the transport by smartHTTP, not in the repository's config.
func (f *Fake) initRepo(name string) (*repo, error) {
	f.nextID++
	rp := &repo{
		id: f.nextID, name: name, dir: filepath.Join(f.root, f.Owner, name+".git"), created: f.now(),
		issues: map[int64]*issue{}, calls: map[string]int{}, nextIndex: 1,
	}
	if err := os.MkdirAll(rp.dir, 0o750); err != nil {
		return nil, err
	}
	if _, err := rp.git(nil, nil, "init", "--bare", "--quiet", "--initial-branch=main"); err != nil {
		return nil, err
	}
	seed := sig{Name: "Gitea Fake", Email: "fake@gitea.invalid", When: f.now()}
	sha, err := rp.commit("", "Initial commit", seed, seed, []fileOp{
		{op: "create", path: "README.md", content: []byte("# " + name + "\n")},
	})
	if err != nil {
		return nil, err
	}
	if err := rp.setRef("main", sha, ""); err != nil {
		return nil, err
	}
	return rp, nil
}

// fileOp is one ChangeFiles operation after validation.
type fileOp struct {
	op       string // create, update, delete, rename
	path     string
	fromPath string
	content  []byte
}

// resolve returns the commit a branch, tag or commit SHA names.
func (rp *repo) resolve(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	for _, c := range []string{"refs/heads/" + ref, "refs/tags/" + ref} {
		if out, err := rp.git(nil, nil, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return strings.TrimSpace(out), true
		}
	}
	if isHex(ref) && len(ref) >= 7 {
		if out, err := rp.git(nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return strings.TrimSpace(out), true
		}
	}
	return "", false
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return s != ""
}

func (rp *repo) branchExists(name string) bool {
	_, err := rp.git(nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// branches lists branch names and their head commits, sorted by name.
func (rp *repo) branches() ([][2]string, error) {
	out, err := rp.git(nil, nil, "for-each-ref", "--sort=refname", "--format=%(refname:short) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var res [][2]string
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if n, sha, ok := strings.Cut(l, " "); ok {
			res = append(res, [2]string{n, sha})
		}
	}
	return res, nil
}

func (rp *repo) defaultBranch() string {
	out, err := rp.git(nil, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "main"
	}
	return strings.TrimSpace(out)
}

// commitInfo parses a commit object.
func (rp *repo) commitInfo(sha string) (gitCommit, bool) {
	out, err := rp.git(nil, nil, "cat-file", "commit", sha)
	if err != nil {
		return gitCommit{}, false
	}
	c := gitCommit{SHA: sha}
	head, msg, _ := strings.Cut(out, "\n\n")
	c.Message = msg
	for l := range strings.SplitSeq(head, "\n") {
		k, v, _ := strings.Cut(l, " ")
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author = parseSig(v)
		case "committer":
			c.Committer = parseSig(v)
		}
	}
	return c, true
}

func parseSig(s string) sig {
	lt, gt := strings.LastIndex(s, "<"), strings.LastIndex(s, ">")
	if lt < 0 || gt < lt {
		return sig{Name: s}
	}
	g := sig{Name: strings.TrimSpace(s[:lt]), Email: s[lt+1 : gt]}
	if fs := strings.Fields(s[gt+1:]); len(fs) > 0 {
		if n, err := strconv.ParseInt(fs[0], 10, 64); err == nil {
			g.When = time.Unix(n, 0).UTC()
		}
	}
	return g
}

// lsTree lists a tree-ish. With recursive, trees are listed too (ls-tree -r -t).
func (rp *repo) lsTree(treeish string, recursive bool, paths ...string) ([]entry, error) {
	args := []string{"ls-tree", "-z", "-l"}
	if recursive {
		args = append(args, "-r", "-t")
	}
	args = append(args, treeish)
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := rp.git(nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var res []entry
	for rec := range strings.SplitSeq(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fs := strings.Fields(meta)
		if len(fs) < 4 {
			continue
		}
		e := entry{Mode: fs[0], Type: fs[1], SHA: fs[2], Path: path}
		if fs[3] != "-" {
			e.Size, _ = strconv.ParseInt(fs[3], 10, 64)
		}
		res = append(res, e)
	}
	return res, nil
}

func (rp *repo) blob(sha string) ([]byte, error) {
	out, err := rp.git(nil, nil, "cat-file", "blob", sha)
	return []byte(out), err
}

// commit writes a commit that applies ops to the tree of parent ("" for a root commit). It
// does not move any ref.
func (rp *repo) commit(parent, msg string, author, committer sig, ops []fileOp) (string, error) {
	idx, err := os.CreateTemp("", "fakegitea-index-")
	if err != nil {
		return "", err
	}
	idxPath := idx.Name()
	_ = idx.Close()
	_ = os.Remove(idxPath) // git creates it
	defer func() { _ = os.Remove(idxPath) }()
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if parent != "" {
		if _, err := rp.git(nil, env, "read-tree", parent); err != nil {
			return "", err
		}
	}
	put := func(path string, content []byte, mode string) error {
		blob, err := rp.git(content, nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		_, err = rp.git(nil, env, "update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(blob)+","+path)
		return err
	}
	remove := func(path string) error {
		// Mode 0 removes the entry; --force-remove would need a work tree.
		_, err := rp.git([]byte("0 "+strings.Repeat("0", 40)+"\t"+path+"\n"), env, "update-index", "--index-info")
		return err
	}
	var existing map[string]entry
	if parent != "" {
		es, err := rp.lsTree(parent, true)
		if err != nil {
			return "", err
		}
		existing = map[string]entry{}
		for _, e := range es {
			existing[e.Path] = e
		}
	}
	modeOf := func(path string) string {
		if e, ok := existing[path]; ok && e.Type == "blob" {
			return e.Mode
		}
		return "100644"
	}
	for _, o := range ops {
		var err error
		switch o.op {
		case "create", "update":
			err = put(o.path, o.content, modeOf(o.path))
		case "delete":
			err = remove(o.path)
		case "rename":
			mode := modeOf(o.fromPath)
			content := o.content
			if content == nil {
				if content, err = rp.blob(existing[o.fromPath].SHA); err != nil {
					return "", err
				}
			}
			if err = remove(o.fromPath); err == nil {
				err = put(o.path, content, mode)
			}
		}
		if err != nil {
			return "", err
		}
	}
	tree, err := rp.git(nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	sha, err := rp.git(nil, identityEnv(author, committer), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// setRef points refs/heads/<branch> at sha; old is the expected current value ("" for a new
// branch), so a concurrent push is not overwritten.
func (rp *repo) setRef(branch, sha, old string) error {
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	_, err := rp.git(nil, nil, "update-ref", "refs/heads/"+branch, sha, old)
	return err
}

// branchHead returns the commit a branch (not a tag or SHA) points at.
func (rp *repo) branchHead(name string) (string, bool) {
	out, err := rp.git(nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+name+"^{commit}")
	return strings.TrimSpace(out), err == nil
}
