// SPDX-License-Identifier: Apache-2.0

package approvals

// Pushing saved work (plan item AG1; ADR 0054).
//
// When a stuck sandbox is recreated, koinon-agentd seals the workspace's uncommitted work in
// Postgres and asks, as the system, for the approval "Push saved work to a branch" (a request
// with system set, tool push_saved_work). It is an ordinary ask-tier request: an approver sees
// the diffstat, approves the hash of the arguments, and only then does Koinon open the sealed
// files and write them to GitHub through the same connection and the same Git Data calls as a
// pull request - blobs, a tree on top of the recorded base commit, one commit, and a new branch
// koinon/wip/<task>-<time>. The default branch is never written, an existing branch is never
// overwritten, and a repository the run's agent version is not bound to, or the installation
// cannot reach, is refused. The request ID is the idempotency key, as for pull requests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// savedWorkPermissions are the installation token's permissions for the push: contents only.
var savedWorkPermissions = map[string]string{"metadata": "read", "contents": "write"}

// executeSavedWork pushes the approved saved work to its branch.
func (s *Service) executeSavedWork(ctx context.Context, p codehost.Provider, row store.ApprovalRequest) (map[string]any, error) {
	args, err := savedwork.DecodePushArgs(row.ArgsJson)
	if err != nil {
		return nil, permanentError{err}
	}
	var id pgtype.UUID
	if err := id.Scan(args.SavedWorkID); err != nil || !id.Valid {
		return nil, permanent("the request names no saved work")
	}
	if s.savedKeys == nil {
		return nil, permanent("saved work cannot be opened: this server has no key-encryption keys")
	}
	sw, err := s.q.GetSavedWork(ctx, store.GetSavedWorkParams{OrgID: row.OrgID, ID: id})
	if err != nil {
		return nil, permanent("the saved work is gone")
	}
	switch {
	case sw.RunID != row.RunID || sw.TaskID != row.TaskID, sw.Repo != args.Repo, sw.BaseSha != args.Base, sw.Branch != args.Branch:
		return nil, permanent("the request does not match the saved work")
	case sw.Status == "expired" || !sw.ExpiresAt.Time.After(time.Now()):
		return nil, permanent("the saved work expired (it is kept for 14 days)")
	case sw.Status != "saved" || len(sw.Envelope) == 0:
		return nil, permanent("the saved work was already pushed")
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(sw.Envelope, &env); err != nil {
		return nil, permanent("the saved work is unreadable")
	}
	work, packed, err := savedwork.OpenPacked(s.savedKeys, row.OrgID.String(), id.String(), env)
	if err != nil {
		return nil, permanent("the saved work cannot be opened: %v", err)
	}
	// The approval binds to the content the approver's card showed: the packed files and the
	// manifest (also kept in the clear on the row, which the card reads).
	var shown savedwork.Manifest
	if err := json.Unmarshal(sw.Manifest, &shown); err != nil {
		return nil, permanent("the saved work's manifest is unreadable")
	}
	switch {
	case work.Manifest.Repo != args.Repo || work.Manifest.Base != args.Base:
		return nil, permanent("the saved work does not match its manifest")
	case savedwork.BundleHash(packed) != args.BundleSHA256:
		return nil, permanent("the saved files are not the ones that were approved (bundle hash differs)")
	case savedwork.ManifestHash(work.Manifest) != args.ManifestSHA256, savedwork.ManifestHash(shown) != args.ManifestSHA256:
		return nil, permanent("the saved files are not the ones that were approved (manifest hash differs)")
	}
	repo, err := p.ParseRepo(args.Repo)
	if err != nil {
		return nil, permanentError{err}
	}
	conn, err := s.connectionFor(ctx, p, row, args.Repo, false)
	if err != nil {
		return nil, err
	}
	acc, err := gitpush.Open(ctx, p, conn, codehost.Scope{Repositories: []string{repo.String()}, Permissions: savedWorkPermissions}, s.dial)
	if err != nil {
		return nil, err
	}
	info, err := acc.Repository(ctx, repo)
	if err != nil {
		return nil, err
	}
	if args.Branch == info.DefaultBranch {
		return nil, permanent("saved work is never written to the default branch")
	}
	head, err := s.existingHead(ctx, acc, repo, args.Branch, row.ID.String())
	if err != nil {
		return nil, err
	}
	if head == "" {
		requested, approved, err := s.people(ctx, row)
		if err != nil {
			return nil, err
		}
		task, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: row.OrgID, ID: row.TaskID})
		if err != nil {
			return nil, err
		}
		msg := savedWorkMessage(task.Title, row.ID.String(), requested, approved)
		if head, err = s.commitSaved(ctx, p, acc, repo, work, msg); err != nil {
			return nil, err
		}
		err = acc.CreateBranch(ctx, repo, args.Branch, head)
		if errors.Is(err, codehost.ErrBranchExists) {
			if head, err = s.existingHead(ctx, acc, repo, args.Branch, row.ID.String()); err == nil && head == "" {
				err = permanent("branch %s appeared while it was being created", args.Branch)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"url": p.BranchURL(conn, repo, args.Branch), "branch": args.Branch, "base": args.Base,
		"commit": head, "repo": args.Repo, "files": len(work.Manifest.Files),
	}, nil
}

// savedWorkMessage is the commit message of pushed saved work, with the trailers.
func savedWorkMessage(taskTitle, approvalID, requestedBy, approvedBy string) string {
	var b strings.Builder
	b.WriteString("WIP: work saved from a stuck sandbox\n\n")
	fmt.Fprintf(&b, "Koinon saved this uncommitted work when the agent's sandbox had to be recreated.\nTask: %s\n\n", oneLine(taskTitle))
	fmt.Fprintf(&b, "%s: %s\n%s: %s\n%s: %s\n", TrailerRequestedBy, requestedBy, TrailerApprovedBy, approvedBy, TrailerApproval, approvalID)
	return b.String()
}

// commitSaved writes the commit with the saved files on top of the base commit, and returns its
// SHA. The base commit must be on the code host.
func (s *Service) commitSaved(ctx context.Context, p codehost.Provider, acc codehost.Access, repo codehost.Repo, w savedwork.Work, message string) (string, error) {
	_, err := acc.Commit(ctx, repo, w.Manifest.Base)
	if codehost.IsStatus(err, 404) || codehost.IsStatus(err, 422) {
		return "", permanent("the commit the work is based on (%s) is not on %s in %s; push the branch it came from first", w.Manifest.Base, p.Capabilities().DisplayName, repo)
	}
	if err != nil {
		return "", err
	}
	files := make([]codehost.FileWrite, 0, len(w.Manifest.Files))
	for _, f := range w.Manifest.Files {
		mode := f.Mode
		if f.Status == savedwork.Deleted {
			mode = "100644"
		}
		fw := codehost.FileWrite{Path: f.Path, Mode: mode, Delete: f.Status == savedwork.Deleted}
		if !fw.Delete {
			fw.Content = w.Content[f.Path]
		}
		files = append(files, fw)
	}
	return acc.WriteCommit(ctx, repo, codehost.CommitRequest{Base: w.Manifest.Base, Message: message, Files: files})
}
