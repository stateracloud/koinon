// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"fmt"
	"maps"
	"strings"
)

// AddRepo creates a repository with a main branch holding README.md. It panics if git fails.
func (f *Fake) AddRepo(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.repos[strings.ToLower(name)]; ok {
		return
	}
	rp, err := f.initRepo(name)
	if err != nil {
		panic(fmt.Errorf("fakegitea: create %s: %w", name, err))
	}
	f.repos[strings.ToLower(name)] = rp
}

// AddIssue creates an issue and returns its number, which pull requests share.
func (f *Fake) AddIssue(repoName, title string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp, err := f.lookup(f.Owner, repoName)
	if err != nil {
		panic(err)
	}
	i := &issue{index: rp.nextIndex, title: title, user: DefaultLogin, created: f.now()}
	rp.nextIndex++
	rp.issues[i.index] = i
	return i.index
}

// AddComment adds a comment by the default login to issue or pull request index and returns its
// ID. It panics if the repository does not exist.
func (f *Fake) AddComment(repoName string, index int64, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp, err := f.lookup(f.Owner, repoName)
	if err != nil {
		panic(err)
	}
	rp.nextCID++
	rp.comments = append(rp.comments, &comment{id: rp.nextCID, index: index, user: DefaultLogin, body: body, created: f.now()})
	return rp.nextCID
}

// RepoDir is the bare repository's directory, for tests that inspect it with git.
func (f *Fake) RepoDir(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp, err := f.lookup(f.Owner, name)
	if err != nil {
		panic(err)
	}
	return rp.dir
}

// PullState is a pull request as seen by tests.
type PullState struct {
	Number      int64
	Title, Body string
	Head, Base  string
	State       string
}

// CommentState is an issue comment as seen by tests.
type CommentState struct {
	ID     int64
	Number int64
	User   string
	Body   string
}

// Snapshot is a repository's state, for assertions and the fake binary's /_fake endpoint.
type Snapshot struct {
	Name          string
	DefaultBranch string
	Branches      map[string]string // branch -> commit SHA
	Pulls         []PullState
	Comments      []CommentState
	// Calls counts write calls by endpoint, e.g. "POST pulls", and the repository-wide comment
	// list's reads as "GET repo comments".
	Calls map[string]int
}

// Snapshot returns a copy of the repository's state. It panics if the repository is unknown.
func (f *Fake) Snapshot(name string) Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp, err := f.lookup(f.Owner, name)
	if err != nil {
		panic(err)
	}
	s := Snapshot{Name: rp.name, DefaultBranch: rp.defaultBranch(), Branches: map[string]string{}, Calls: maps.Clone(rp.calls)}
	bs, _ := rp.branches()
	for _, b := range bs {
		s.Branches[b[0]] = b[1]
	}
	for _, p := range rp.pulls {
		s.Pulls = append(s.Pulls, PullState{Number: p.index, Title: p.title, Body: p.body, Head: p.head, Base: p.base, State: p.state})
	}
	for _, c := range rp.comments {
		s.Comments = append(s.Comments, CommentState{ID: c.id, Number: c.index, User: c.user, Body: c.body})
	}
	return s
}

// SetVisibility makes a repository "public", or "internal" (not private, in a private
// organization: Gitea reports private=false, internal=true), or private again ("private").
func (f *Fake) SetVisibility(repoName, visibility string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.public == nil {
		f.public = map[string]string{}
	}
	if visibility == "private" {
		delete(f.public, strings.ToLower(repoName))
		return
	}
	f.public[strings.ToLower(repoName)] = visibility
}
