// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"path"
	"strings"

	"github.com/stateracloud/koinon/internal/acp"
)

// Tier is the risk tier of an action (ADR 0049).
type Tier string

// Tiers of the actions a harness asks permission for over ACP.
const (
	// TierRead reads inside the sandbox.
	TierRead Tier = "read"
	// TierSandboxWrite changes the sandbox's own files or runs commands in it. Its effects
	// stay in the sandbox, whose policy bounds what a command can reach.
	TierSandboxWrite Tier = "sandbox-write"
)

// Verdict is how a tier is answered.
type Verdict string

// Verdicts.
const (
	Allow  Verdict = "allow"
	Reject Verdict = "reject"
)

// PermissionPolicy answers a harness's session/request_permission. A request that names a path
// outside the session directory is rejected whatever the mapping says, since harnesses report
// such reads with ordinary kinds (OpenCode v2: kind "read"). Otherwise the tool call's kind
// maps to a tier and the tier to a verdict; a kind or tier without an entry is rejected.
//
// Harness permission requests are about actions inside the sandbox. Actions with effects
// outside (a pull request, an issue comment) are MCP tools behind agentgateway, whose
// approval tiers Koinon enforces there (plan items C3, P1); they never reach this policy.
type PermissionPolicy struct {
	Tiers    map[acp.ToolKind]Tier
	Verdicts map[Tier]Verdict
}

// DefaultPermissions is v0.1's mapping. The sandbox is the boundary (ADR 0049, spike step
// 7), so reads and sandbox-local changes are allowed; fetches, mode switches and tool calls
// of unknown kind are rejected.
func DefaultPermissions() *PermissionPolicy {
	return &PermissionPolicy{
		Tiers: map[acp.ToolKind]Tier{
			acp.ToolKindRead:    TierRead,
			acp.ToolKindSearch:  TierRead,
			acp.ToolKindThink:   TierRead,
			acp.ToolKindEdit:    TierSandboxWrite,
			acp.ToolKindDelete:  TierSandboxWrite,
			acp.ToolKindMove:    TierSandboxWrite,
			acp.ToolKindExecute: TierSandboxWrite,
		},
		Verdicts: map[Tier]Verdict{
			TierRead:         Allow,
			TierSandboxWrite: Allow,
		},
	}
}

// decision is a policy answer and why.
type decision struct {
	// Outside is the first path the request names outside the session directory.
	Outside  string       `json:"outsideWorkspace,omitempty"`
	Kind     acp.ToolKind `json:"kind,omitempty"`
	Tier     Tier         `json:"tier,omitempty"`
	Verdict  Verdict      `json:"verdict"`
	OptionID string       `json:"optionId,omitempty"`
}

// decide answers req for a session working in workspace. Allowing picks a one-time allow
// option, so the harness asks again next time; rejecting picks a reject option, or cancels
// the request if none is offered.
func (p *PermissionPolicy) decide(req acp.PermissionRequest, workspace string) decision {
	var d decision
	if req.ToolCall.Kind != nil {
		d.Kind = *req.ToolCall.Kind
	}
	d.Tier = p.Tiers[d.Kind]
	d.Verdict = p.Verdicts[d.Tier]
	if d.Tier == "" || d.Verdict == "" {
		d.Verdict = Reject
	}
	for _, l := range req.ToolCall.Locations {
		if !within(workspace, l.Path) {
			d.Outside, d.Verdict = l.Path, Reject
			break
		}
	}
	var opt acp.Decision
	if d.Verdict == Allow {
		opt = acp.Choose(req, acp.AllowOnce, acp.AllowAlways)
		if opt.OptionID == "" {
			// Nothing to allow with: answer as a rejection.
			d.Verdict = Reject
		}
	}
	if d.Verdict == Reject {
		opt = acp.Choose(req, acp.RejectOnce, acp.RejectAlways)
	}
	d.OptionID = opt.OptionID
	return d
}

// within reports whether p, absolute or relative to dir, is dir or below it.
func within(dir, p string) bool {
	dir = path.Clean(dir)
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	p = path.Clean(p)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
