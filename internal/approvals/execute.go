// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/mcpask"
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

// execute runs the approved call against GitHub and returns its result.
func (s *Service) execute(ctx context.Context, row store.ApprovalRequest) (map[string]any, error) {
	if row.Connector == mcpask.ConnectorID {
		return s.executeMCP(ctx, row)
	}
	if row.Connector != connectors.GitHubID {
		return nil, permanent("no executor for connector %q", row.Connector)
	}
	if row.System && row.Tool == savedwork.ToolPush {
		return s.executeSavedWork(ctx, row)
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
	repo, app, err := s.target(ctx, row)
	if err != nil {
		return nil, err
	}
	tok, err := s.gh.InstallationToken(ctx, app, github.TokenRequest{Repositories: []string{repo.String()}, Permissions: tool.Permissions})
	if err != nil {
		return nil, err
	}
	switch args := call.(type) {
	case *githubtools.PullRequestArgs:
		requested, approved, err := s.people(ctx, row)
		if err != nil {
			return nil, err
		}
		return s.createPullRequest(ctx, tok, repo, *args, row.ID.String(), requested, approved)
	case *githubtools.CommentArgs:
		return s.comment(ctx, tok, repo, *args, row.ID.String())
	default:
		return nil, permanent("no executor for tool %q", call.Tool())
	}
}

// target resolves the request's repository and the GitHub App of the run's connection. The
// repository must still be the one the run's agent version is bound to.
func (s *Service) target(ctx context.Context, row store.ApprovalRequest) (github.Repo, github.App, error) {
	full, ok := strings.CutPrefix(row.Target, connectors.GitHubID+":")
	if !ok {
		return github.Repo{}, github.App{}, permanent("target %q is not a GitHub repository", row.Target)
	}
	repo, err := github.ParseRepo(full)
	if err != nil {
		return github.Repo{}, github.App{}, permanentError{err}
	}
	app, err := s.appFor(ctx, row, full, true)
	return repo, app, err
}

// appFor returns the GitHub App of the connection the run's agent version binds full to. With
// single, the binding must name exactly that repository (a pull request or comment); without, it
// may name more (saved work is pushed to any repository the agent may use).
func (s *Service) appFor(ctx context.Context, row store.ApprovalRequest, full string, single bool) (github.App, error) {
	run, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: row.OrgID, ID: row.RunID})
	if err != nil {
		return github.App{}, err
	}
	v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: row.OrgID, ID: run.AgentVersionID})
	if err != nil {
		return github.App{}, err
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return github.App{}, permanentError{err}
	}
	b, ok := cfg.Binding(connectors.GitHubID)
	if !ok || (single && len(b.Targets) != 1) || !slices.ContainsFunc(b.Targets, func(t string) bool { return strings.EqualFold(t, full) }) {
		return github.App{}, permanent("the run's agent version is not bound to %s", full)
	}
	var connID pgtype.UUID
	if err := connID.Scan(b.ConnectionID); err != nil || !connID.Valid {
		return github.App{}, permanent("the agent's GitHub binding names no connection")
	}
	conn, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: row.OrgID, ID: connID})
	if err != nil {
		return github.App{}, fmt.Errorf("load the GitHub connection: %w", err)
	}
	app, err := connections.LoadGitHubApp(ctx, s.keys, conn)
	if err != nil {
		if errors.Is(err, connections.ErrNoKeyring) || errors.Is(err, connections.ErrRemoved) {
			return github.App{}, permanentError{err}
		}
		return github.App{}, err
	}
	return app, nil
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
	if row.DecidedBy.Valid {
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
func (s *Service) createPullRequest(ctx context.Context, tok github.Token, repo github.Repo, args githubtools.PullRequestArgs,
	approvalID, requestedBy, approvedBy string,
) (map[string]any, error) {
	base := args.Base
	if base == "" {
		r, err := s.gh.GetRepository(ctx, tok, repo)
		if err != nil {
			return nil, err
		}
		base = r.DefaultBranch
	}
	head, err := s.existingHead(ctx, tok, repo, args.Branch, approvalID)
	if err != nil {
		return nil, err
	}
	if head == "" {
		if head, err = s.commit(ctx, tok, repo, args, base, CommitMessage(args, approvalID, requestedBy, approvedBy)); err != nil {
			return nil, err
		}
		err = s.gh.CreateBranch(ctx, tok, repo, args.Branch, head)
		if errors.Is(err, github.ErrRefExists) {
			// A concurrent attempt won the race; use its branch if it is ours.
			if head, err = s.existingHead(ctx, tok, repo, args.Branch, approvalID); err == nil && head == "" {
				err = permanent("branch %s appeared while it was being created", args.Branch)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	pr, found, err := s.findPullRequest(ctx, tok, repo, args.Branch)
	if err != nil {
		return nil, err
	}
	if !found {
		pr, err = s.gh.CreatePullRequest(ctx, tok, repo, args.Title, args.Body, args.Branch, base)
		if github.IsStatus(err, 422) {
			// "A pull request already exists": a concurrent attempt created it.
			var again bool
			if pr, again, err = s.findPullRequest(ctx, tok, repo, args.Branch); err == nil && !again {
				err = permanent("GitHub refused the pull request from %s into %s", args.Branch, base)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"url": pr.HTMLURL, "number": pr.Number, "branch": args.Branch, "base": base, "commit": head,
	}, nil
}

// existingHead returns the branch's head if the branch exists and was created for this
// approval, "" if it does not exist, and a permanent error if it belongs to someone else.
func (s *Service) existingHead(ctx context.Context, tok github.Token, repo github.Repo, branch, approvalID string) (string, error) {
	sha, found, err := s.gh.GetBranch(ctx, tok, repo, branch)
	if err != nil || !found {
		return "", err
	}
	c, err := s.gh.GetCommit(ctx, tok, repo, sha)
	if err != nil {
		return "", err
	}
	if hasTrailer(c.Message, approvalID) {
		return sha, nil
	}
	// A later commit on the branch (someone pushed to it) still counts if its parent chain
	// starts at ours; only the first parent is checked.
	if len(c.Parents) == 1 {
		p, err := s.gh.GetCommit(ctx, tok, repo, c.Parents[0].SHA)
		if err != nil {
			return "", err
		}
		if hasTrailer(p.Message, approvalID) {
			return sha, nil
		}
	}
	return "", permanent("branch %s already exists in %s and was not created for this approval; ask the agent for another branch name", branch, repo)
}

// commit creates the blobs, the tree on top of base and the commit, and returns its SHA.
func (s *Service) commit(ctx context.Context, tok github.Token, repo github.Repo, args githubtools.PullRequestArgs, base, message string) (string, error) {
	baseSHA, found, err := s.gh.GetBranch(ctx, tok, repo, base)
	if err != nil {
		return "", err
	}
	if !found {
		return "", permanent("base branch %s does not exist in %s", base, repo)
	}
	baseCommit, err := s.gh.GetCommit(ctx, tok, repo, baseSHA)
	if err != nil {
		return "", err
	}
	modes, err := s.baseModes(ctx, tok, repo, baseCommit.Tree.SHA, args.Files)
	if err != nil {
		return "", err
	}
	entries := make([]github.TreeEntry, 0, len(args.Files))
	for _, f := range args.Files {
		e := github.TreeEntry{Path: f.Path, Mode: FileMode(f, modes[f.Path]), Type: "blob"}
		if !f.Delete {
			sha, err := s.gh.CreateBlob(ctx, tok, repo, []byte(f.Content))
			if err != nil {
				return "", err
			}
			e.SHA = &sha
		}
		entries = append(entries, e)
	}
	tree, err := s.gh.CreateTree(ctx, tok, repo, baseCommit.Tree.SHA, entries)
	if err != nil {
		return "", err
	}
	return s.gh.CreateCommit(ctx, tok, repo, message, tree, []string{baseSHA})
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

// baseModes returns the mode in the base tree of each file the call writes that exists
// there as a blob, keyed by path; an absent path is new. One recursive listing answers it;
// if GitHub truncated that listing (a very large repository), the paths it did not return
// are looked up directory by directory. The calls are bounded by the 100-file cap.
func (s *Service) baseModes(ctx context.Context, tok github.Token, repo github.Repo, tree string, files []githubtools.FileChange) (map[string]string, error) {
	want := map[string]bool{}
	for _, f := range files {
		if !f.Delete {
			want[f.Path] = true
		}
	}
	modes := map[string]string{}
	if len(want) == 0 {
		return modes, nil
	}
	all, err := s.gh.GetTree(ctx, tok, repo, tree, true)
	if err != nil {
		return nil, err
	}
	for _, e := range all.Tree {
		if e.Type == "blob" && want[e.Path] {
			modes[e.Path] = e.Mode
		}
	}
	if !all.Truncated {
		return modes, nil
	}
	// dirs caches each directory's listing ("" is the root); nil when it does not exist.
	dirs := map[string][]github.TreeItem{}
	var list func(dir string) ([]github.TreeItem, error)
	list = func(dir string) ([]github.TreeItem, error) {
		if items, ok := dirs[dir]; ok {
			return items, nil
		}
		sha := tree
		if dir != "" {
			parent, name := path.Split(dir)
			items, err := list(strings.TrimSuffix(parent, "/"))
			if err != nil {
				return nil, err
			}
			sha = ""
			for _, e := range items {
				if e.Path == name && e.Type == "tree" {
					sha = e.SHA
				}
			}
			if sha == "" {
				dirs[dir] = nil
				return nil, nil
			}
		}
		t, err := s.gh.GetTree(ctx, tok, repo, sha, false)
		if err != nil {
			return nil, err
		}
		dirs[dir] = t.Tree
		return t.Tree, nil
	}
	for p := range want {
		if _, ok := modes[p]; ok {
			continue
		}
		dir, name := path.Split(p)
		items, err := list(strings.TrimSuffix(dir, "/"))
		if err != nil {
			return nil, err
		}
		for _, e := range items {
			if e.Path == name && e.Type == "blob" {
				modes[p] = e.Mode
			}
		}
	}
	return modes, nil
}

func (s *Service) findPullRequest(ctx context.Context, tok github.Token, repo github.Repo, branch string) (github.PullRequest, bool, error) {
	prs, err := s.gh.ListPullRequestsForBranch(ctx, tok, repo, branch)
	if err != nil {
		return github.PullRequest{}, false, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return pr, true, nil
		}
	}
	return github.PullRequest{}, false, nil
}

// CommentMarker is appended to the comments Koinon posts: the idempotency key, invisible in
// the rendered comment.
func CommentMarker(approvalID string) string {
	return "<!-- koinon-approval: " + approvalID + " -->"
}

// comment posts the approved comment unless an earlier attempt already did.
func (s *Service) comment(ctx context.Context, tok github.Token, repo github.Repo, args githubtools.CommentArgs, approvalID string) (map[string]any, error) {
	marker := CommentMarker(approvalID)
	existing, err := s.gh.ListIssueComments(ctx, tok, repo, args.IssueNumber)
	if err != nil {
		return nil, err
	}
	for _, c := range existing {
		if strings.Contains(c.Body, marker) {
			return map[string]any{"url": c.HTMLURL, "comment_id": c.ID, "issue_number": args.IssueNumber}, nil
		}
	}
	c, err := s.gh.CreateIssueComment(ctx, tok, repo, args.IssueNumber, args.Body+"\n\n"+marker)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": c.HTMLURL, "comment_id": c.ID, "issue_number": args.IssueNumber}, nil
}
