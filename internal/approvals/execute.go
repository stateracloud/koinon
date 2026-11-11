// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/gitpush"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/savedwork"
)

// Trailers of the commits Koinon creates. TrailerApproval carries the idempotency key.
const (
	TrailerRequestedBy = "Requested-by"
	TrailerApprovedBy  = "Approved-by"
	TrailerApproval    = "Koinon-Approval"
)

// trustedApprover names the approver of a call the Trusted preset allowed.
const trustedApprover = "Koinon Trusted preset (no human approval)"

// execute runs the approved call against the code host and returns its result.
func (s *Service) execute(ctx context.Context, row store.ApprovalRequest) (map[string]any, error) {
	if row.Connector == mcpask.ConnectorID {
		return s.executeMCP(ctx, row)
	}
	p, ok := s.hosts.Get(row.Connector)
	if !ok {
		return nil, permanent("no executor for connector %q", row.Connector)
	}
	if row.System && row.Tool == savedwork.ToolPush {
		return s.executeSavedWork(ctx, p, row)
	}
	tool, err := githubtools.LookupWriteTool(row.Tool)
	if err != nil {
		return nil, permanentError{err}
	}
	// Defence in depth: the tool server validated the call before recording it.
	call, err := tool.Decode([]byte(row.ArgsJson))
	if err != nil {
		return nil, permanentError{err}
	}
	repo, conn, err := s.target(ctx, p, row)
	if err != nil {
		return nil, err
	}
	acc, err := gitpush.Open(ctx, p, conn, codehost.Scope{Repositories: []string{repo.String()}, Permissions: tool.Permissions})
	if err != nil {
		return nil, err
	}
	switch args := call.(type) {
	case *githubtools.PullRequestArgs:
		requested, approved, err := s.people(ctx, row)
		if err != nil {
			return nil, err
		}
		// Defence in depth: the tool server applied the guard when it recorded the call, but the
		// connection's CI directories may have changed since, and the arguments are re-read here.
		guard, err := codehost.GuardOf(p.Capabilities(), conn.Config)
		if err != nil {
			return nil, permanentError{err}
		}
		if err := githubtools.ValidatePullRequest(*args, guard); err != nil {
			return nil, permanentError{err}
		}
		return s.createPullRequest(ctx, p, acc, repo, *args, guard, row.ID.String(), requested, approved)
	case *githubtools.CommentArgs:
		return s.comment(ctx, acc, repo, *args, row.ID.String())
	default:
		return nil, permanent("no executor for tool %q", call.Tool())
	}
}

// target resolves the request's repository and the run's connection. The repository must still
// be the one the run's agent version is bound to.
func (s *Service) target(ctx context.Context, p codehost.Provider, row store.ApprovalRequest) (codehost.Repo, codehost.Connection, error) {
	full, ok := strings.CutPrefix(row.Target, p.ID()+":")
	if !ok {
		return codehost.Repo{}, codehost.Connection{}, permanent("target %q is not a %s repository", row.Target, p.Capabilities().DisplayName)
	}
	repo, err := p.ParseRepo(full)
	if err != nil {
		return codehost.Repo{}, codehost.Connection{}, permanentError{err}
	}
	conn, err := s.connectionFor(ctx, p, row, full, true)
	return repo, conn, err
}

// connectionFor returns the connection the run's agent version binds full to. With single, the
// binding must name exactly that repository (a pull request or comment); without, it may name
// more (saved work is pushed to any repository the agent may use).
func (s *Service) connectionFor(ctx context.Context, p codehost.Provider, row store.ApprovalRequest, full string, single bool) (codehost.Connection, error) {
	name := p.Capabilities().DisplayName
	run, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: row.OrgID, ID: row.RunID})
	if err != nil {
		return codehost.Connection{}, err
	}
	v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: row.OrgID, ID: run.AgentVersionID})
	if err != nil {
		return codehost.Connection{}, err
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return codehost.Connection{}, permanentError{err}
	}
	b, ok := cfg.Binding(p.ID())
	if !ok || (single && len(b.Targets) != 1) || !slices.ContainsFunc(b.Targets, func(t string) bool { return strings.EqualFold(t, full) }) {
		return codehost.Connection{}, permanent("the run's agent version is not bound to %s", full)
	}
	var connID pgtype.UUID
	if err := connID.Scan(b.ConnectionID); err != nil || !connID.Valid {
		return codehost.Connection{}, permanent("the agent's %s binding names no connection", name)
	}
	row2, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: row.OrgID, ID: connID})
	if err != nil {
		return codehost.Connection{}, fmt.Errorf("load the %s connection: %w", name, err)
	}
	conn, err := connections.Load(ctx, s.keys, row2)
	if err != nil {
		if errors.Is(err, connections.ErrNoKeyring) || errors.Is(err, connections.ErrRemoved) ||
			errors.Is(err, connections.ErrCredentialLost) || errors.Is(err, outward.ErrDisabled) {
			return codehost.Connection{}, permanentError{err}
		}
		return codehost.Connection{}, err
	}
	return conn, nil
}

// people names the task's creator and the approver for the commit trailers.
func (s *Service) people(ctx context.Context, row store.ApprovalRequest) (requested, approved string, err error) {
	t, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: row.OrgID, ID: row.TaskID})
	if err != nil {
		return "", "", err
	}
	requested = "Koinon"
	if t.CreatedByUserID.Valid {
		if requested, err = s.person(ctx, t.CreatedByUserID); err != nil {
			return "", "", err
		}
	}
	approved = trustedApprover
	if row.ScopeID.Valid {
		// A scoped approval: the trailer names the admin who granted the scope.
		sc, err := s.q.GetApprovalScope(ctx, store.GetApprovalScopeParams{OrgID: row.OrgID, ID: row.ScopeID})
		if err != nil {
			return "", "", err
		}
		who, err := s.person(ctx, sc.CreatedBy)
		if err != nil {
			return "", "", err
		}
		approved = who + " (scoped approval)"
	} else if row.DecidedBy.Valid {
		if approved, err = s.person(ctx, row.DecidedBy); err != nil {
			return "", "", err
		}
	}
	return requested, approved, nil
}

func (s *Service) person(ctx context.Context, id pgtype.UUID) (string, error) {
	u, err := s.q.GetActiveUser(ctx, id)
	if err != nil {
		return "Koinon user " + id.String(), nil //nolint:nilerr // a deleted user still gets a trailer
	}
	name := oneLine(u.DisplayName)
	email := oneLine(u.Email.String)
	switch {
	case name != "" && email != "":
		return name + " <" + email + ">", nil
	case email != "":
		return "<" + email + ">", nil
	case name != "":
		return name, nil
	}
	return "Koinon user " + id.String(), nil
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, s))
}

// CommitMessage is the message of the commit Koinon creates for an approved pull request:
// the title, the body, and the trailers.
func CommitMessage(args githubtools.PullRequestArgs, approvalID, requestedBy, approvedBy string) string {
	var b strings.Builder
	b.WriteString(args.Title)
	b.WriteString("\n\n")
	if body := strings.TrimSpace(args.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "%s: %s\n%s: %s\n%s: %s\n", TrailerRequestedBy, requestedBy, TrailerApprovedBy, approvedBy, TrailerApproval, approvalID)
	return b.String()
}

func hasTrailer(message, approvalID string) bool {
	want := TrailerApproval + ": " + approvalID
	for line := range strings.Lines(message) {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// createPullRequest creates the branch, one commit with exactly the approved files on top
// of the base, and the pull request. What an earlier attempt of the same approval created
// is found first and reused.
func (s *Service) createPullRequest(ctx context.Context, p codehost.Provider, acc codehost.Access, repo codehost.Repo, args githubtools.PullRequestArgs, guard codehost.Guard,
	approvalID, requestedBy, approvedBy string,
) (map[string]any, error) {
	base := args.Base
	if base == "" {
		r, err := acc.Repository(ctx, repo)
		if err != nil {
			return nil, err
		}
		base = r.DefaultBranch
	}
	head, err := s.existingHead(ctx, acc, repo, args.Branch, approvalID)
	if err != nil {
		return nil, err
	}
	if head == "" {
		if head, err = s.commit(ctx, acc, repo, args, guard, base, CommitMessage(args, approvalID, requestedBy, approvedBy)); err != nil {
			return nil, err
		}
		err = acc.CreateBranch(ctx, repo, args.Branch, head)
		if errors.Is(err, codehost.ErrBranchExists) {
			// A concurrent attempt won the race; use its branch if it is ours.
			if head, err = s.existingHead(ctx, acc, repo, args.Branch, approvalID); err == nil && head == "" {
				err = permanent("branch %s appeared while it was being created", args.Branch)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	pr, found, err := s.findPullRequest(ctx, acc, repo, args.Branch)
	if err != nil {
		return nil, err
	}
	if !found {
		pr, err = acc.CreatePullRequest(ctx, repo, codehost.PullRequestInput{Title: args.Title, Body: args.Body, Head: args.Branch, Base: base})
		if codehost.IsStatus(err, 422, 409) {
			// "A pull request already exists" (GitHub: 422, Gitea: 409): a concurrent attempt
			// created it.
			var again bool
			if pr, again, err = s.findPullRequest(ctx, acc, repo, args.Branch); err == nil && !again {
				err = permanent("%s refused the pull request from %s into %s", p.Capabilities().DisplayName, args.Branch, base)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"url": pr.URL, "number": pr.Number, "branch": args.Branch, "base": base, "commit": head,
	}, nil
}

// existingHead returns the branch's head if the branch exists and was created for this
// approval, "" if it does not exist, and a permanent error if it belongs to someone else.
func (s *Service) existingHead(ctx context.Context, acc codehost.Access, repo codehost.Repo, branch, approvalID string) (string, error) {
	sha, found, err := acc.Branch(ctx, repo, branch)
	if err != nil || !found {
		return "", err
	}
	c, err := acc.Commit(ctx, repo, sha)
	if err != nil {
		return "", err
	}
	if hasTrailer(c.Message, approvalID) {
		return sha, nil
	}
	// A later commit on the branch (someone pushed to it) still counts if its parent chain
	// starts at ours; only the first parent is checked.
	if len(c.Parents) == 1 {
		p, err := acc.Commit(ctx, repo, c.Parents[0])
		if err != nil {
			return "", err
		}
		if hasTrailer(p.Message, approvalID) {
			return sha, nil
		}
	}
	return "", permanent("branch %s already exists in %s and was not created for this approval; ask the agent for another branch name", branch, repo)
}

// commit writes the commit with exactly the approved files on top of base, and returns its SHA.
func (s *Service) commit(ctx context.Context, acc codehost.Access, repo codehost.Repo, args githubtools.PullRequestArgs, guard codehost.Guard, base, message string) (string, error) {
	baseSHA, found, err := acc.Branch(ctx, repo, base)
	if err != nil {
		return "", err
	}
	if !found {
		return "", permanent("base branch %s does not exist in %s", base, repo)
	}
	modes, err := acc.Modes(ctx, repo, baseSHA, writtenPaths(args.Files))
	if err != nil {
		return "", err
	}
	files := make([]codehost.FileWrite, 0, len(args.Files))
	for _, f := range args.Files {
		w := codehost.FileWrite{Path: f.Path, Mode: FileMode(f, modes[f.Path]), Delete: f.Delete}
		if !f.Delete {
			w.Content = []byte(f.Content)
		}
		files = append(files, w)
	}
	// The files as written: paths, modes and symbolic-link targets are checked once more.
	if err := guard.CheckFiles(files); err != nil {
		return "", permanentError{err}
	}
	return acc.WriteCommit(ctx, repo, codehost.CommitRequest{Base: baseSHA, Message: message, Files: files})
}

// writtenPaths are the paths of the files the call writes (not deletes).
func writtenPaths(files []githubtools.FileChange) []string {
	var out []string
	for _, f := range files {
		if !f.Delete {
			out = append(out, f.Path)
		}
	}
	return out
}

// FileMode is the git mode Koinon writes a file of an approved pull request with, given
// its mode on the base tree ("" for a new file): 100755 when the call marks it executable
// or it already was executable, else 100644. Only regular and executable blobs are
// written: the arguments cannot express a symbolic link (120000) or a submodule (160000),
// so a modified symbolic link becomes a regular file.
func FileMode(f githubtools.FileChange, base string) string {
	if f.Executable || base == "100755" {
		return "100755"
	}
	return "100644"
}

func (s *Service) findPullRequest(ctx context.Context, acc codehost.Access, repo codehost.Repo, branch string) (codehost.PullRequest, bool, error) {
	prs, err := acc.PullRequests(ctx, repo, branch)
	if err != nil {
		return codehost.PullRequest{}, false, err
	}
	for _, pr := range prs {
		if pr.HeadRef == branch {
			return pr, true, nil
		}
	}
	return codehost.PullRequest{}, false, nil
}

// CommentMarker is appended to the comments Koinon posts: the idempotency key, invisible in
// the rendered comment.
func CommentMarker(approvalID string) string {
	return "<!-- koinon-approval: " + approvalID + " -->"
}

// comment posts the approved comment unless an earlier attempt already did.
func (s *Service) comment(ctx context.Context, acc codehost.Access, repo codehost.Repo, args githubtools.CommentArgs, approvalID string) (map[string]any, error) {
	marker := CommentMarker(approvalID)
	existing, err := acc.Comments(ctx, repo, args.IssueNumber)
	if err != nil {
		return nil, err
	}
	for _, c := range existing {
		if strings.Contains(c.Body, marker) {
			return map[string]any{"url": c.URL, "comment_id": c.ID, "issue_number": args.IssueNumber}, nil
		}
	}
	c, err := acc.CreateComment(ctx, repo, args.IssueNumber, args.Body+"\n\n"+marker)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": c.URL, "comment_id": c.ID, "issue_number": args.IssueNumber}, nil
}
