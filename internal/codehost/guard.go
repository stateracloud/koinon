// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxCIDirs bounds the CI directories configured on one connection.
const MaxCIDirs = 20

// maxCIDirBytes bounds one configured CI directory.
const maxCIDirBytes = 200

// Guard is the workflow guard of a connection (ADR 0062): the paths whose changes run CI on the
// provider, which a proposed commit must not touch. It is the provider's CI paths from connector
// data (spec.ciPaths) plus the directories configured on the connection (a Gitea server's
// WORKFLOW_DIRS, say). The zero Guard refuses nothing.
//
// An entry guards itself and everything under it. Matching is exact, or case-insensitive when the
// provider says its file system is (spec.ciPathsFoldCase). Paths are cleaned first, so ./ and ..
// segments cannot hide a guarded path. A commit has no rename: a move is a delete plus a write,
// and each side is checked, so a move into or out of a guarded directory is refused.
type Guard struct {
	raw  []string // as declared, for messages and the approval card
	dirs []string // cleaned, no trailing slash, folded when fold
	fold bool
}

// NewGuard builds the guard of a provider (its CI paths and case rule) and a connection's
// configured directories. Invalid configured directories are an error.
func NewGuard(caps Capabilities, configured []string) (Guard, error) {
	g := Guard{fold: caps.CIPathsFoldCase}
	add := func(raw, clean string) {
		if g.fold {
			clean = strings.ToLower(clean)
		}
		for _, d := range g.dirs {
			if d == clean {
				return
			}
		}
		g.raw = append(g.raw, raw)
		g.dirs = append(g.dirs, clean)
	}
	for _, p := range caps.CIPaths {
		clean := cleanGuardPath(p)
		if clean == "" {
			continue // connector data is validated when it is parsed; skip what cannot match
		}
		add(p, clean)
	}
	for _, p := range configured {
		clean, err := CleanCIDir(p)
		if err != nil {
			return Guard{}, err
		}
		add(clean, clean)
	}
	return g, nil
}

// GuardOf builds the guard of a connection from its provider's capabilities and its stored
// config (the ciDirs key).
func GuardOf(caps Capabilities, config []byte) (Guard, error) {
	site, err := SiteOf(config)
	if err != nil {
		return Guard{}, err
	}
	return NewGuard(caps, site.CIDirs)
}

// CleanCIDir validates one configured CI directory and returns it cleaned: relative to the
// repository root, no .. segments, no control characters. A trailing slash is dropped.
func CleanCIDir(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", fmt.Errorf("a CI directory is empty")
	case len(s) > maxCIDirBytes:
		return "", fmt.Errorf("CI directory %.40q… is longer than %d bytes", s, maxCIDirBytes)
	case !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl):
		return "", fmt.Errorf("CI directory %q contains control characters or invalid UTF-8", s)
	case strings.Contains(s, `\`):
		return "", fmt.Errorf("CI directory %q contains a backslash; use / as the separator", s)
	case strings.HasPrefix(s, "/"):
		return "", fmt.Errorf("CI directory %q is absolute; write it relative to the repository root", s)
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", fmt.Errorf("CI directory %q leaves the repository", s)
		}
	}
	clean := path.Clean(s)
	if clean == "." {
		return "", fmt.Errorf("CI directory %q is the repository root; list the directories that hold CI configuration", s)
	}
	return clean, nil
}

// CleanCIDirs validates a list of configured CI directories and returns them cleaned and
// without duplicates.
func CleanCIDirs(in []string) ([]string, error) {
	if len(in) > MaxCIDirs {
		return nil, fmt.Errorf("at most %d CI directories", MaxCIDirs)
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		c, err := CleanCIDir(s)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// cleanGuardPath cleans a path for matching against the guard's entries: separators unified,
// leading ./ and / dropped, . and .. resolved. A path that climbs out of the root is anchored
// at the root (../x is x), the conservative reading.
func cleanGuardPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimLeft(path.Clean("/"+p), "/")
	return p
}

// Paths are the entries as the approval card shows them: the provider's CI paths and the
// connection's directories.
func (g Guard) Paths() []string { return append([]string(nil), g.raw...) }

// FoldCase is whether matching ignores case.
func (g Guard) FoldCase() bool { return g.fold }

// Refuse returns the guarded entry that p falls under, or "" when p is free to change.
func (g Guard) Refuse(p string) string {
	c := cleanGuardPath(p)
	if g.fold {
		c = strings.ToLower(c)
	}
	for i, d := range g.dirs {
		if under(c, d) {
			return g.raw[i]
		}
	}
	return ""
}

func under(c, dir string) bool { return c == dir || strings.HasPrefix(c, dir+"/") }

// CheckFiles refuses a commit that touches a guarded path: every written, deleted or moved
// path, and a symbolic link (mode 120000) whose target is a guarded path or a directory that
// holds one, since a link makes the guarded files reachable through another name. It reports
// every problem.
func (g Guard) CheckFiles(files []FileWrite) error {
	if len(g.dirs) == 0 {
		return nil
	}
	var msgs []string
	for _, f := range files {
		if hit := g.Refuse(f.Path); hit != "" {
			msgs = append(msgs, fmt.Sprintf("path %q is under %s, which runs CI on the code host; Koinon never proposes CI changes: say on the task that the change needs one and stop", f.Path, hit))
			continue
		}
		if f.Delete || f.Mode != "120000" {
			continue
		}
		if why := g.symlinkProblem(f.Path, string(f.Content)); why != "" {
			msgs = append(msgs, why)
		}
	}
	if len(msgs) > 0 {
		return &GuardError{Reasons: msgs}
	}
	return nil
}

// symlinkProblem says why the symbolic link at p with the given target reaches a guarded path.
func (g Guard) symlinkProblem(p, target string) string {
	target = strings.ReplaceAll(target, `\`, "/")
	if target == "" || strings.HasPrefix(target, "/") {
		return fmt.Sprintf("symbolic link %q has an empty or absolute target, which could reach any path", p)
	}
	resolved := path.Join(path.Dir(cleanGuardPath(p)), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Sprintf("symbolic link %q points outside the repository", p)
	}
	if g.fold {
		resolved = strings.ToLower(resolved)
	}
	for i, d := range g.dirs {
		// Into a guarded path, or at a directory that holds one (".", ".github").
		if under(resolved, d) || resolved == "." || strings.HasPrefix(d, resolved+"/") {
			return fmt.Sprintf("symbolic link %q points at %q, which reaches %s", p, target, g.raw[i])
		}
	}
	return ""
}

// GuardError is a commit the workflow guard refuses.
type GuardError struct{ Reasons []string }

func (e *GuardError) Error() string { return "refused: " + strings.Join(e.Reasons, "; ") }
