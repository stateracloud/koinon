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
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-git/go-git/v5/utils/ioutil"

	"github.com/stateracloud/koinon/connectors"
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
	// CABundle is PEM certificates that sign the host's TLS certificate, besides the system
	// roots: the connection's caBundle (ForSite).
	CABundle []byte
	// DialContext is the egress policy's dial check (catalog.URLPolicy.DialContext): the fetch, the
	// ref listing and the push connect only to addresses it allows, checked when the connection is
	// made (after the host name resolves), and refuse HTTP redirects. Nil dials unchecked; redirects
	// are refused either way.
	DialContext DialFunc
	// Username is the HTTP basic user name; the token is the password. Default "koinon".
	Username string
	// AuthorName and AuthorEmail identify the commits. Default "Koinon" and
	// "noreply@<project domain>".
	AuthorName, AuthorEmail string
	// Now is the commit time; default time.Now.
	Now func() time.Time
}

// ForSite returns the options for a connection on the host at site: the remote URLs are built
// from the connection's base URL (the connector's default host when it has none) and its CA
// bundle is trusted for the fetch and the push (ADR 0062), so a Gitea or GHES host with a private
// CA can be pushed to. Username, author and clock keep their defaults; set them on the result.
func ForSite(t *connectors.Type, site codehost.Site) (Options, error) {
	loc, err := site.Location(t)
	if err != nil {
		return Options{}, err
	}
	if _, err := site.Pool(); err != nil {
		return Options{}, err
	}
	base := loc.BaseURL()
	o := Options{RemoteURL: func(r codehost.Repo) string { return base + "/" + r.String() + ".git" }}
	if strings.TrimSpace(site.CABundle) != "" {
		o.CABundle = []byte(site.CABundle)
	}
	return o, nil
}

// Wrap returns inner when the provider writes through its API, and an Access that writes by
// git push when its Capabilities say Writes is "push".
func Wrap(p codehost.Provider, inner codehost.Access, o Options) codehost.Access {
	if p.Capabilities().Writes != codehost.WritesPush {
		return inner
	}
	return New(inner, o)
}

// Open opens p's Access for scope on conn and, for a provider that writes by git push, wraps it
// with the push writer built from the connection's base URL and CA bundle. It is the one call
// koinon-api's executors use, so a provider's write path follows its connector data.
func Open(ctx context.Context, p codehost.Provider, conn codehost.Connection, scope codehost.Scope, dial DialFunc) (codehost.Access, error) {
	acc, err := p.Open(ctx, conn, scope)
	if err != nil {
		return nil, err
	}
	if p.Capabilities().Writes != codehost.WritesPush {
		return acc, nil
	}
	t, ok := connectors.Get(p.ID())
	if !ok {
		return nil, fmt.Errorf("gitpush: the %s connector type is not embedded", p.ID())
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return nil, err
	}
	o, err := ForSite(t, site)
	if err != nil {
		return nil, fmt.Errorf("gitpush: connection %s: %w", conn.ID, err)
	}
	o.DialContext = dial
	return Wrap(p, acc, o), nil
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
	return &access{Access: inner, o: o, tr: newTransport(o.DialContext), pending: map[string]pending{}}
}

type pending struct {
	repo codehost.Repo
	st   *memory.Storage
}

type access struct {
	codehost.Access
	o  Options
	tr transport.Transport // the policy-checked, redirect-refusing HTTP transport

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
	treeHash, empty, err := rewrite(st, tree, ops)
	if err != nil {
		return "", err
	}
	if empty {
		// Everything was deleted: the commit's tree is the empty tree (4b825dc...).
		if treeHash, _, err = writeTree(st, nil); err != nil {
			return "", err
		}
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
	url := a.o.RemoteURL(r)
	fetch := func(spec string) error { return a.fetchShallow(ctx, st, url, spec) }
	err := fetch(sha)
	if err == nil {
		return nil
	}
	refs, lerr := a.listRefs(ctx, url)
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
	err = a.createRef(ctx, r, p.st, plumbing.NewBranchReferenceName(name), plumbing.NewHash(sha))
	return a.scrub(err)
}

// createRef sends one create command for want, with the old value the zero hash, so the host
// refuses it under its ref lock when the ref exists at any value, even one the new commit
// descends from. go-git's PushContext would fill the old value in from the refs advertised at
// push time and fast-forward such a branch, so the update is sent here instead.
func (a *access) createRef(ctx context.Context, r codehost.Repo, st *memory.Storage, want plumbing.ReferenceName, sha plumbing.Hash) (err error) {
	ep, err := a.endpoint(a.o.RemoteURL(r))
	if err != nil {
		return err
	}
	sess, err := a.tr.NewReceivePackSession(ep, a.auth())
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(sess, &err)
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return fmt.Errorf("gitpush: listing refs of %s: %w", r, err)
	}
	if ar == nil {
		ar = packp.NewAdvRefs()
	}
	remoteRefs, err := ar.AllReferences()
	if err != nil {
		return err
	}
	if ref, err := remoteRefs.Reference(want); err == nil && ref.Type() == plumbing.HashReference {
		return codehost.ErrBranchExists
	}

	haves, err := referencesToHashes(remoteRefs)
	if err != nil {
		return err
	}
	stop, err := st.Shallow()
	if err != nil {
		return err
	}
	haves = append(haves, stop...)
	hashes, err := revlist.Objects(st, []plumbing.Hash{sha}, haves)
	if err != nil {
		return err
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, st, !ar.Capabilities.Supports(capability.OFSDelta)).Encode(hashes, 10); err != nil {
		return err
	}

	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: want, Old: plumbing.ZeroHash, New: sha}}
	req.Packfile = io.NopCloser(&pack)
	status, err := sess.ReceivePack(ctx, req)
	if err == nil && status != nil {
		err = status.Error()
	}
	if err != nil {
		// A rejection because the ref exists (a branch created since the listing) and one for
		// another reason read differently by host; look at the ref to tell them apart.
		if exists, lerr := a.refExists(ctx, r, want); lerr == nil && exists {
			return codehost.ErrBranchExists
		}
		return fmt.Errorf("gitpush: pushing %s to %s: %w", want.Short(), r, err)
	}
	return nil
}

// refExists lists the refs of r and reports whether name is among them.
func (a *access) refExists(ctx context.Context, r codehost.Repo, name plumbing.ReferenceName) (bool, error) {
	refs, err := a.listRefs(ctx, a.o.RemoteURL(r))
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.Name() == name {
			return true, nil
		}
	}
	return false, nil
}

func referencesToHashes(refs storer.ReferenceStorer) ([]plumbing.Hash, error) {
	iter, err := refs.IterReferences()
	if err != nil {
		return nil, err
	}
	var hs []plumbing.Hash
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			hs = append(hs, ref.Hash())
		}
		return nil
	})
	return hs, err
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
		e, existed := entries[name]
		isDir := existed && e.Mode == filemode.Dir
		if isDir {
			if sub, err = object.GetTree(st, e.Hash); err != nil {
				return hash, false, err
			}
		}
		if !isDir && allDeletes(g) {
			// Nothing below a file or a missing entry to delete; leave the entry alone.
			continue
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
			// Only a directory can be emptied; a file the ops wrote over is replaced below.
			delete(entries, name)
			continue
		}
		entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: sh}
	}
	if len(entries) == 0 {
		return hash, true, nil
	}
	return writeTree(st, entries)
}

func allDeletes(g []fileOp) bool {
	for _, op := range g {
		if !op.delete {
			return false
		}
	}
	return true
}

func writeTree(st storer.EncodedObjectStorer, entries map[string]object.TreeEntry) (hash plumbing.Hash, empty bool, err error) {
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
