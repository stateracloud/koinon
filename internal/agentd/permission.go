// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

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

// pathResolver returns the physical path of each of paths in the sandbox (symbolic links
// resolved), one per path, with "!" for a path that is itself a symbolic link or cannot be
// resolved. workspace is resolved the same way and comes back first.
type pathResolver func(workspace string, paths []string) ([]string, error)

// decide answers req for a session working in workspace. Allowing picks a one-time allow
// option, so the harness asks again next time; agentd never picks an "always" option, since
// agentd answers each request on its own and nothing the harness remembers may widen access
// (old issue 260). A request without a once option is answered with a cancel. Rejecting picks a
// reject-once option, or cancels the request if none is offered.
//
// The paths are checked lexically first. When resolve is not nil, the paths that pass are
// resolved in the sandbox (a link inside the workspace that points outside is outside), and a
// request whose paths cannot be resolved is rejected.
func (p *PermissionPolicy) decide(req acp.PermissionRequest, workspace string, resolve pathResolver) decision {
	var d decision
	if req.ToolCall.Kind != nil {
		d.Kind = *req.ToolCall.Kind
	}
	d.Tier = p.Tiers[d.Kind]
	d.Verdict = p.Verdicts[d.Tier]
	if d.Tier == "" || d.Verdict == "" {
		d.Verdict = Reject
	}
	var paths []string
	for _, l := range req.ToolCall.Locations {
		if !within(workspace, l.Path) || strings.ContainsAny(l.Path, "\n\x00") {
			d.Outside, d.Verdict = l.Path, Reject
			break
		}
		// Resolved as the harness names it: cleaning first would drop a ".." that follows a link,
		// which the kernel resolves from the link's target.
		paths = append(paths, rawAbsPath(workspace, l.Path))
	}
	if d.Verdict == Allow && resolve != nil && len(paths) > 0 {
		if out := outsideAfterLinks(workspace, paths, resolve); out != "" {
			d.Outside, d.Verdict = out, Reject
		}
	}
	var opt acp.Decision
	if d.Verdict == Allow {
		opt = acp.Choose(req, acp.AllowOnce)
		if opt.OptionID == "" {
			// Nothing to allow with: answer as a rejection.
			d.Verdict = Reject
		}
	}
	if d.Verdict == Reject {
		opt = acp.Choose(req, acp.RejectOnce)
	}
	d.OptionID = opt.OptionID
	return d
}

// outsideAfterLinks returns the first of paths (absolute, not cleaned, lexically inside workspace) whose
// physical path is not inside the workspace's, or "" when all are. A path that is itself a
// symbolic link counts as outside whatever it points at, and so does any path that cannot be
// resolved: the check fails closed.
func outsideAfterLinks(workspace string, paths []string, resolve pathResolver) string {
	got, err := resolve(workspace, paths)
	if err != nil || len(got) != len(paths)+1 || got[0] == "!" {
		return paths[0]
	}
	for i, p := range paths {
		if got[i+1] == "!" || !within(got[0], got[i+1]) {
			return p
		}
	}
	return ""
}

// rawAbsPath is p made absolute against dir, not cleaned.
func rawAbsPath(dir, p string) string {
	if path.IsAbs(p) {
		return p
	}
	return strings.TrimSuffix(dir, "/") + "/" + p
}

// absPath is p made absolute against dir and cleaned.
func absPath(dir, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	return path.Clean(p)
}

// realpathScript prints the physical path of each argument after the first (the workspace,
// printed first), "!" for one that is itself a symbolic link or cannot be resolved. A path that
// does not exist yet is its deepest existing ancestor's physical path plus the rest (which has
// no links, so the caller may clean it lexically). The paths come as arguments, uncleaned, so a
// ".." after a link is resolved from the link's target as the kernel does. It fails when the
// workspace cannot be resolved.
const realpathScript = `# koinon-realpath
phys() {
  d=$1; rest=
  while [ ! -e "$d" ] && [ ! -L "$d" ]; do
    rest="/${d##*/}$rest"; d=${d%/*}; [ -n "$d" ] || d=/
  done
  if [ -L "$d" ]; then echo '!'; return; fi
  if [ -d "$d" ]; then b=$(cd -P -- "$d" 2>/dev/null && pwd -P) || { echo '!'; return; }
  else
    b=$(cd -P -- "${d%/*}/" 2>/dev/null && pwd -P) || { echo '!'; return; }
    b=${b%/}/${d##*/}
  fi
  b=${b%/}$rest
  echo "${b:-/}"
}
ws=$(cd -P -- "$1" 2>/dev/null && pwd -P) || exit 1
echo "$ws"
shift
for p; do phys "$p"; done`

// within reports whether p, absolute or relative to dir, is dir or below it. It is lexical: see
// outsideAfterLinks for symbolic links.
func within(dir, p string) bool {
	dir = path.Clean(dir)
	p = absPath(dir, p)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// pathResolver resolves the paths of a permission request in the sandbox, one exec per request
// that names a location. It returns nil (lexical checks only) when the sandbox is not known.
func (r *runner) pathResolver(ctx context.Context) pathResolver {
	if r.sbName == "" {
		return nil
	}
	return func(workspace string, paths []string) ([]string, error) {
		cmd := append([]string{"/bin/sh", "-c", realpathScript, "sh", workspace}, paths...)
		out, code, stderr, err := r.sandboxExecLimit(ctx, r.sbWorkspace, r.sbName, cmd, nil, 10*time.Second, 1<<20)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr))
		}
		if err != nil {
			// The request is rejected (fail closed); say why, since the refusal names the path.
			r.log.Warn("could not resolve a permission request's paths in the sandbox", "err", err)
			return nil, err
		}
		return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), nil
	}
}
