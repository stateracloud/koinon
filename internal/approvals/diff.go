// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/githubtools"
)

// ErrNoDiff: the request is not a create_pull_request, so it has no files to compare.
var ErrNoDiff = errors.New("only create_pull_request requests have a diff")

// diffPermissions is the token Diff mints: read the repository, nothing else.
var diffPermissions = map[string]string{"metadata": "read", "contents": "read"}

// maxBaseFileBytes is the largest current version Diff compares, and maxBaseTotalBytes
// what all of them may total: the tool's 1 MiB cap (githubtools.MaxTotalBytes), applied to
// the other side of the diff.
const (
	maxBaseFileBytes  = githubtools.MaxTotalBytes
	maxBaseTotalBytes = githubtools.MaxTotalBytes
)

// FileStatus is what the change does to one file.
type FileStatus int

// File statuses. StatusUnknown is a file whose current version could not be read.
const (
	StatusUnknown FileStatus = iota
	StatusAdded
	StatusModified
	StatusDeleted
)

// FileDiff compares one file of a proposed pull request with its current version on the
// base branch.
type FileDiff struct {
	Path   string
	Status FileStatus
	// Diff is a unified diff (--- a/path, +++ b/path, hunks with three lines of context);
	// empty when the file is unchanged, binary, or Fallback is set.
	Diff string
	// Binary: the new or the current version is not text; no content is shown.
	Binary bool
	// Fallback says why there is no diff; the card then shows the full new content.
	Fallback string
	// OldMode is the file's git mode on the base branch and NewMode the mode Koinon writes
	// (FileMode): set when the base tree could be read, OldMode for an existing file and
	// NewMode for every file that is not deleted. A modified file whose modes differ
	// becomes executable (100644 -> 100755).
	OldMode, NewMode string
}

// Diff is a create_pull_request request compared with the branch it targets.
type Diff struct {
	// Base is the branch the pull request will target: the call's base, or the
	// repository's default branch. Empty if it could not be resolved.
	Base  string
	Files []FileDiff
}

// Diff reads the current version of each file of a create_pull_request request at the head
// of its base branch, through the run's GitHub connection with a contents:read token for
// the one repository, and compares it with the proposed content. It never fails because of
// GitHub: a file whose current version cannot be compared (GitHub error, larger than 1
// MiB, not a file) gets a Fallback reason instead, and so does every file when the
// repository or the base cannot be read. row must already be authorized for the caller.
func (s *Service) Diff(ctx context.Context, row store.ApprovalRequest) (Diff, error) {
	if row.Tool != githubtools.ToolCreatePullRequest {
		return Diff{}, ErrNoDiff
	}
	// The same descriptor the executor uses: decode and validate the recorded call.
	call, err := githubtools.DecodeCall(row.Tool, []byte(row.ArgsJson))
	if err != nil {
		return Diff{}, err
	}
	args, ok := call.(*githubtools.PullRequestArgs)
	if !ok {
		return Diff{}, ErrNoDiff
	}
	out := Diff{Base: args.Base, Files: make([]FileDiff, len(args.Files))}
	for i, f := range args.Files {
		out.Files[i].Path = f.Path
		if f.Delete {
			out.Files[i].Status = StatusDeleted
		}
	}
	fallback := func(err error) Diff {
		reason := s.fallbackReason(row, err)
		for i := range out.Files {
			out.Files[i].Fallback = reason
		}
		return out
	}
	if row.Connector != connectors.GitHubID {
		return fallback(permanent("no GitHub connector for %q", row.Connector)), nil
	}
	repo, app, err := s.target(ctx, row)
	if err != nil {
		return fallback(err), nil
	}
	tok, err := s.gh.InstallationToken(ctx, app, github.TokenRequest{Repositories: []string{repo.String()}, Permissions: diffPermissions})
	if err != nil {
		return fallback(err), nil
	}
	if out.Base == "" {
		r, err := s.gh.GetRepository(ctx, tok, repo)
		if err != nil {
			return fallback(err), nil
		}
		out.Base = r.DefaultBranch
	}
	// One commit for every file, so the files compare against the same snapshot.
	head, found, err := s.gh.GetBranch(ctx, tok, repo, out.Base)
	if err == nil && !found {
		err = permanent("the base branch %s does not exist in %s", out.Base, repo)
	}
	if err != nil {
		return fallback(err), nil
	}
	s.modes(ctx, row, tok, repo, head, args.Files, out.Files)
	budget := maxBaseTotalBytes
	for i, f := range args.Files {
		d := &out.Files[i]
		cur, found, err := s.gh.GetFileContent(ctx, tok, repo, f.Path, head)
		switch {
		case err != nil:
			d.Fallback = s.fallbackReason(row, err)
			continue
		case !found && f.Delete:
			d.Fallback = fmt.Sprintf("%s does not exist on %s", f.Path, out.Base)
			continue
		case !found:
			d.Status = StatusAdded
		case cur.Type != "file":
			d.Fallback = fmt.Sprintf("%s is a %s on %s, not a file", f.Path, cur.Type, out.Base)
			continue
		case cur.Size > maxBaseFileBytes || cur.Content == nil:
			d.Fallback = fmt.Sprintf("the current version is larger than 1 MiB (%d bytes)", cur.Size)
			continue
		case len(cur.Content) > budget:
			d.Fallback = "the current versions of the files total more than 1 MiB"
			continue
		default:
			budget -= len(cur.Content)
			if !f.Delete {
				d.Status = StatusModified
			}
		}
		if githubtools.LooksBinary(f.Path, f.Content) || githubtools.LooksBinary(f.Path, string(cur.Content)) {
			d.Binary = true
			continue
		}
		d.Diff = UnifiedDiff(f.Path, string(cur.Content), f.Content, d.Status)
	}
	return out, nil
}

// modes fills each file's OldMode and NewMode from the base commit's tree, the same read
// the executor makes. A tree that cannot be read leaves them empty; the diff itself does
// not depend on it.
func (s *Service) modes(ctx context.Context, row store.ApprovalRequest, tok github.Token, repo github.Repo, head string, files []githubtools.FileChange, out []FileDiff) {
	c, err := s.gh.GetCommit(ctx, tok, repo, head)
	var base map[string]string
	if err == nil {
		base, err = s.baseModes(ctx, tok, repo, c.Tree.SHA, files)
	}
	if err != nil {
		s.log.Warn("approval diff: could not read the base modes", append(attrs(row), "err", err)...)
		return
	}
	for i, f := range files {
		if f.Delete {
			continue
		}
		out[i].OldMode = base[f.Path]
		out[i].NewMode = FileMode(f, base[f.Path])
	}
}

// fallbackReason is the short note the card shows instead of a diff. GitHub's own message
// is logged, not shown.
func (s *Service) fallbackReason(row store.ApprovalRequest, err error) string {
	var p permanentError
	if errors.As(err, &p) {
		return p.Error()
	}
	s.log.Warn("approval diff: could not read the current version", append(attrs(row), "err", err)...)
	var apiErr *github.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("GitHub answered %d %s when Koinon read the current version", apiErr.Status, http.StatusText(apiErr.Status))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "reading the current version from GitHub timed out"
	}
	return "Koinon could not read the current version from GitHub"
}

// UnifiedDiff is the unified diff from old to new for path, with three lines of context,
// headed by --- a/path and +++ b/path (/dev/null for an added or deleted file). A last
// line without a newline is marked "\ No newline at end of file", as git does. Equal
// contents give "".
func UnifiedDiff(path, old, new string, status FileStatus) string {
	from, to := "a/"+path, "b/"+path
	switch status {
	case StatusAdded:
		from = "/dev/null"
	case StatusDeleted:
		to = "/dev/null"
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: diffLines(old), B: diffLines(new), FromFile: from, ToFile: to, Context: 3,
	})
	if err != nil {
		return "" // a strings.Builder-backed writer does not fail
	}
	return out
}

// diffLines splits s into lines that each end in "\n". A last line without one gets the
// no-newline marker as its own line, so "x" and "x\n" differ.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	lines[len(lines)-1] += "\n\\ No newline at end of file\n"
	return lines
}
