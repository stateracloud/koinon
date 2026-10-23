// SPDX-License-Identifier: Apache-2.0

package githubtools

import (
	"encoding/json"
	"fmt"

	approvalv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/approval/v1alpha1"
)

// WriteTool describes one ask-tier tool. The tool server, the API and the executor all look
// tools up here, so a tool that is not described is unknown everywhere.
type WriteTool struct {
	// Name is the MCP tool name.
	Name string
	// Scope is the grant scope the tool needs.
	Scope string
	// Permissions are the installation token permissions executing the call needs: the least
	// it needs. Every installation token can read metadata; asking for it explicitly keeps
	// GitHub's answer equal to the request.
	Permissions map[string]string

	decode func(argsJSON []byte) (Call, error)
}

// Call is the decoded, validated arguments of one write call: a PullRequestArgs or a
// CommentArgs.
type Call interface {
	// Tool is the name of the call's WriteTool.
	Tool() string
	// Validate checks the arguments; every problem is reported.
	Validate() error
	// SetApprovalCall puts the call on an approval message.
	SetApprovalCall(out *approvalv1alpha1.Approval)
}

var writeTools = map[string]WriteTool{
	ToolCreatePullRequest: {
		Name:        ToolCreatePullRequest,
		Scope:       "pull_request.create",
		Permissions: map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"},
		decode:      decodeAs[PullRequestArgs],
	},
	ToolCommentOnIssue: {
		Name:        ToolCommentOnIssue,
		Scope:       "issue.comment",
		Permissions: map[string]string{"metadata": "read", "issues": "write"},
		decode:      decodeAs[CommentArgs],
	},
}

// UnknownToolError is returned for a tool that is not a described write tool.
type UnknownToolError struct{ Tool string }

func (e *UnknownToolError) Error() string { return fmt.Sprintf("unknown GitHub write tool %q", e.Tool) }

// LookupWriteTool returns the write tool called name.
func LookupWriteTool(name string) (WriteTool, error) {
	t, ok := writeTools[name]
	if !ok {
		return WriteTool{}, &UnknownToolError{Tool: name}
	}
	return t, nil
}

// Decode decodes and validates arguments recorded for t.
func (t WriteTool) Decode(argsJSON []byte) (Call, error) {
	return t.decode(argsJSON)
}

// DecodeCall looks up tool and decodes and validates its recorded arguments.
func DecodeCall(tool string, argsJSON []byte) (Call, error) {
	t, err := LookupWriteTool(tool)
	if err != nil {
		return nil, err
	}
	return t.Decode(argsJSON)
}

func decodeAs[T any, P interface {
	*T
	Call
}](argsJSON []byte) (Call, error) {
	var v T
	if err := json.Unmarshal(argsJSON, &v); err != nil {
		return nil, fmt.Errorf("decode the recorded arguments: %w", err)
	}
	c := P(&v)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Tool implements Call.
func (*PullRequestArgs) Tool() string { return ToolCreatePullRequest }

// Validate implements Call.
func (a *PullRequestArgs) Validate() error { return ValidatePullRequest(*a) }

// SetApprovalCall implements Call.
func (a *PullRequestArgs) SetApprovalCall(out *approvalv1alpha1.Approval) {
	pr := &approvalv1alpha1.PullRequestCall{Branch: a.Branch, Base: a.Base, Title: a.Title, Body: a.Body}
	for _, f := range a.Files {
		pr.Files = append(pr.Files, &approvalv1alpha1.FileChange{Path: f.Path, Content: f.Content, Delete: f.Delete, Executable: f.Executable})
	}
	out.Call = &approvalv1alpha1.Approval_PullRequest{PullRequest: pr}
}

// Tool implements Call.
func (*CommentArgs) Tool() string { return ToolCommentOnIssue }

// Validate implements Call.
func (a *CommentArgs) Validate() error { return ValidateComment(*a) }

// SetApprovalCall implements Call.
func (a *CommentArgs) SetApprovalCall(out *approvalv1alpha1.Approval) {
	out.Call = &approvalv1alpha1.Approval_Comment{Comment: &approvalv1alpha1.CommentCall{IssueNumber: a.IssueNumber, Body: a.Body}}
}
