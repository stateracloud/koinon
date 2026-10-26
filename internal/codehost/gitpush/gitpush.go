// SPDX-License-Identifier: Apache-2.0

// Package gitpush writes commits to a code host by git push from koinon-api (ADR 0062): the
// fallback for hosts whose API cannot write a commit with exact file modes, and the only way
// for Gitea.
//
// Wrap puts the writer in front of a provider's Access. Reads go to the provider; WriteCommit
// shallow-fetches the base commit into memory and builds the commit there, and CreateBranch
// pushes that commit as exactly one new branch. Nothing is written to disk. The pushed commit
// is the one WriteCommit returned, so the approval flow (write the commit, create the branch,
// open the pull request) is the same as on the API path, and a replay of an approval finds the
// branch it made before it writes anything.
//
// The writer never pushes the default branch or an existing branch, and uses the connection's
// token only as the HTTP password of the fetch and push.
package gitpush

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/project"
)

// Errors of the writer.
var (
	// ErrProtectedBranch is returned for the default branch and for names that are not a
	// plain branch.
	ErrProtectedBranch = errors.New("gitpush: refusing to push the default branch or a non-branch ref")
	// ErrUnknownCommit is returned by CreateBranch for a commit this Access did not write.
	ErrUnknownCommit = errors.New("gitpush: the commit was not written by this access")
)

// Options configure Wrap.
type Options struct {
	// RemoteURL is the git smart HTTP URL of a repository.
	RemoteURL func(codehost.Repo) string
	// Username is the HTTP basic user name; the token is the password. Default "koinon".
	Username string
	// AuthorName and AuthorEmail identify the commits. Default "Koinon" and
	// "noreply@<project domain>".
	AuthorName, AuthorEmail string
	// Now is the commit time; default time.Now.
	Now func() time.Time
}

// Wrap returns inner when the provider writes through its API, and an Access that writes by
// git push when its Capabilities say Writes is "push".
func Wrap(p codehost.Provider, inner codehost.Access, o Options) codehost.Access {
	if p.Capabilities().Writes != codehost.WritesPush {
		return inner
	}
	return New(inner, o)
}

// New returns an Access that reads through inner and writes by git push.
func New(inner codehost.Access, o Options) codehost.Access {
	if o.Username == "" {
		o.Username = "koinon"
	}
	if o.AuthorName == "" {
		o.AuthorName = "Koinon"
	}
	if o.AuthorEmail == "" {
		o.AuthorEmail = "noreply@" + project.Domain
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &access{Access: inner, o: o, pending: map[string]pending{}}
}

type pending struct {
	repo codehost.Repo
	st   *memory.Storage
}

type access struct {
	codehost.Access
	o Options

	mu      sync.Mutex
	pending map[string]pending
}

var sha1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (a *access) auth() *githttp.BasicAuth {
	return &githttp.BasicAuth{Username: a.o.Username, Password: a.Token().Value()}
}

// scrub removes the token from an error text.
func (a *access) scrub(err error) error {
	if err == nil {
		return nil
	}
	if t := a.Token().Value(); t != "" && strings.Contains(err.Error(), t) {
		return errors.New(strings.ReplaceAll(err.Error(), t, "***"))
	}
	return err
}

func (a *access) remote(st *memory.Storage, r codehost.Repo) *git.Remote {
	return git.NewRemote(st, &config.RemoteConfig{Name: "origin", URLs: []string{a.o.RemoteURL(r)}})
}

// WriteCommit shallow-fetches req.Base, applies the files with their exact modes and builds the
// commit in memory. Nothing reaches the host until CreateBranch.
func (a *access) WriteCommit(ctx context.Context, r codehost.Repo, req codehost.CommitRequest) (string, error) {
	if !sha1Re.MatchString(req.Base) {
		return "", fmt.Errorf("gitpush: base %q is not a full commit SHA", req.Base)
	}
	ops, err := fileOps(req.Files)
	if err != nil {
		return "", err
	}
	st := memory.NewStorage()
	if err := a.fetchBase(ctx, st, r, req.Base); err != nil {
		return "", a.scrub(err)
	}
	base, err := object.GetCommit(st, plumbing.NewHash(req.Base))
	if err != nil {
		return "", fmt.Errorf("gitpush: base commit %s of %s: %w", req.Base, r, err)
	}
	tree, err := base.Tree()
	if err != nil {
		return "", err
	}
	treeHash, _, err := rewrite(st, tree, ops)
	if err != nil {
		return "", err
	}
	sig := object.Signature{Name: a.o.AuthorName, Email: a.o.AuthorEmail, When: a.o.Now()}
	c := &object.Commit{
		Author: sig, Committer: sig, Message: req.Message,
		TreeHash: treeHash, ParentHashes: []plumbing.Hash{base.Hash},
	}
	obj := st.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return "", err
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.pending[h.String()] = pending{repo: r, st: st}
	a.mu.Unlock()
	return h.String(), nil
}

// fetchBase shallow-fetches the commit sha. A host that does not serve commits by SHA (Gitea
// behind git http-backend does not unless configured to) is asked for the ref that points at
// sha instead; the approval flow reads the base branch just before it commits, so the base is
// that branch's tip.
func (a *access) fetchBase(ctx context.Context, st *memory.Storage, r codehost.Repo, sha string) error {
	rem := a.remote(st, r)
	fetch := func(spec string) error {
		err := rem.FetchContext(ctx, &git.FetchOptions{
			RefSpecs: []config.RefSpec{config.RefSpec(spec + ":refs/koinon/base")},
			Depth:    1,
			Tags:     git.NoTags,
			Auth:     a.auth(),
		})
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil
		}
		return err
	}
	err := fetch(sha)
	if err == nil {
		return nil
	}
	refs, lerr := rem.ListContext(ctx, &git.ListOptions{Auth: a.auth()})
	if lerr != nil {
		return fmt.Errorf("gitpush: listing refs of %s: %w", r, lerr)
	}
	for _, ref := range refs {
		if ref.Hash().String() == sha && ref.Name().IsBranch() {
			if err := fetch(ref.Name().String()); err != nil {
				return fmt.Errorf("gitpush: fetching %s of %s: %w", ref.Name(), r, err)
			}
			return nil
		}
	}
	return fmt.Errorf("gitpush: fetching %s of %s: %w", sha, r, err)
}

// CreateBranch pushes sha, which WriteCommit returned, as the new branch name. It refuses the
// default branch and any branch that exists.
func (a *access) CreateBranch(ctx context.Context, r codehost.Repo, name, sha string) error {
	if err := validBranch(name); err != nil {
		return err
	}
	repo, err := a.Repository(ctx, r)
	if err != nil {
		return err
	}
	if name == repo.DefaultBranch {
		return fmt.Errorf("%w: %s", ErrProtectedBranch, name)
	}
	a.mu.Lock()
	p, ok := a.pending[sha]
	a.mu.Unlock()
	if !ok || p.repo != r {
		return fmt.Errorf("%w: %s", ErrUnknownCommit, sha)
	}
	rem := a.remote(p.st, r)
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: a.auth()})
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return a.scrub(fmt.Errorf("gitpush: listing refs of %s: %w", r, err))
	}
	want := plumbing.NewBranchReferenceName(name)
	for _, ref := range refs {
		if ref.Name() == want {
			return codehost.ErrBranchExists
		}
	}
	// Not forced: a branch created since the listing makes the server refuse the push.
	err = rem.PushContext(ctx, &git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(sha + ":" + want.String())},
		Auth:     a.auth(),
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return a.scrub(fmt.Errorf("gitpush: pushing %s to %s: %w", name, r, err))
	}
	return nil
}

func validBranch(name string) error {
	if name == "" || strings.HasPrefix(name, "refs/") || strings.HasPrefix(name, "-") || name == "HEAD" ||
		strings.ContainsAny(name, " ~^:?*[\\\x00") || strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: %q", ErrProtectedBranch, name)
	}
	return plumbing.NewBranchReferenceName(name).Validate()
}

type fileOp struct {
	parts  []string
	mode   filemode.FileMode
	data   []byte
	delete bool
}

func fileOps(files []codehost.FileWrite) ([]fileOp, error) {
	ops := make([]fileOp, 0, len(files))
	for _, f := range files {
		clean := path.Clean(f.Path)
		if f.Path == "" || clean != f.Path || strings.HasPrefix(clean, "/") || clean == "." {
			return nil, fmt.Errorf("gitpush: invalid path %q", f.Path)
		}
		parts := strings.Split(clean, "/")
		for _, p := range parts {
			if p == ".." || strings.EqualFold(p, ".git") {
				return nil, fmt.Errorf("gitpush: invalid path %q", f.Path)
			}
		}
		op := fileOp{parts: parts, delete: f.Delete, data: f.Content}
		if !f.Delete {
			switch f.Mode {
			case "100644":
				op.mode = filemode.Regular
			case "100755":
				op.mode = filemode.Executable
			case "120000":
				op.mode = filemode.Symlink
			default:
				return nil, fmt.Errorf("gitpush: unsupported mode %q for %s", f.Mode, f.Path)
			}
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// rewrite applies ops to tree (nil for an empty directory), writes the new trees and blobs to
// st and returns the new tree's hash; empty is true when nothing is left in it.
func rewrite(st storer.EncodedObjectStorer, tree *object.Tree, ops []fileOp) (hash plumbing.Hash, empty bool, err error) {
	entries := map[string]object.TreeEntry{}
	if tree != nil {
		for _, e := range tree.Entries {
			entries[e.Name] = e
		}
	}
	groups := map[string][]fileOp{}
	for _, op := range ops {
		groups[op.parts[0]] = append(groups[op.parts[0]], op)
	}
	for name, g := range groups {
		if len(g[0].parts) == 1 {
			if len(g) != 1 {
				return hash, false, fmt.Errorf("gitpush: %s is written more than once or as a file and a directory", name)
			}
			op := g[0]
			if op.delete {
				delete(entries, name)
				continue
			}
			bh, err := writeBlob(st, op.data)
			if err != nil {
				return hash, false, err
			}
			entries[name] = object.TreeEntry{Name: name, Mode: op.mode, Hash: bh}
			continue
		}
		var sub *object.Tree
		if e, ok := entries[name]; ok && e.Mode == filemode.Dir {
			if sub, err = object.GetTree(st, e.Hash); err != nil {
				return hash, false, err
			}
		}
		rest := make([]fileOp, len(g))
		for i, op := range g {
			if len(op.parts) == 1 {
				return hash, false, fmt.Errorf("gitpush: %s is written as a file and a directory", name)
			}
			op.parts = op.parts[1:]
			rest[i] = op
		}
		sh, subEmpty, err := rewrite(st, sub, rest)
		if err != nil {
			return hash, false, err
		}
		if subEmpty {
			delete(entries, name)
			continue
		}
		entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: sh}
	}
	if len(entries) == 0 {
		return hash, true, nil
	}
	out := &object.Tree{Entries: make([]object.TreeEntry, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, e)
	}
	// Git orders tree entries by name with a directory's name taken as name+"/".
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	slices.SortFunc(out.Entries, func(a, b object.TreeEntry) int { return cmp.Compare(key(a), key(b)) })
	obj := st.NewEncodedObject()
	if err := out.Encode(obj); err != nil {
		return hash, false, err
	}
	hash, err = st.SetEncodedObject(obj)
	return hash, false, err
}

func writeBlob(st storer.EncodedObjectStorer, data []byte) (plumbing.Hash, error) {
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(obj)
}
