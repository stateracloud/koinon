// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/acp"
)

// A refused permission request must cost the agent one tool call, not the run. ACP's answer to a
// permission request is an option or a cancellation; it carries no message, so the harness cannot
// tell the model why. What a harness then does with a rejection is its own: some hand the model a
// tool error, OpenCode 2.2.0 ends the turn (a plain decline is a defect that interrupts the step:
// packages/core/src/permission.ts "Deliberate defect tunnel", and the turn stops with
// "cancelled"). agentd therefore keeps what it refused during a turn, and when the harness ends
// the turn as cancelled it prompts the same session again with a note that says what was refused
// and why. A harness that continues by itself never reaches the note.

// refusal is a permission request agentd rejected.
type refusal struct {
	Kind  acp.ToolKind `json:"kind,omitempty"`
	Title string       `json:"title,omitempty"`
	// Outside is the path outside the session directory that the request named.
	Outside string `json:"outsideWorkspace,omitempty"`
}

// refused ends a turn: the harness cancelled it after agentd rejected permission requests.
type refused struct{ refusals []refusal }

func (e *refused) Error() string {
	return "the harness cancelled the turn after Koinon refused " + e.describe()
}

func (e *refused) describe() string {
	var parts []string
	for _, f := range e.refusals {
		parts = append(parts, f.describe())
	}
	return strings.Join(parts, "; ")
}

func (f refusal) label() string { return cmp.Or(f.Title, string(f.Kind), "an action") }

func (f refusal) describe() string {
	if f.Outside != "" {
		return fmt.Sprintf("%s on %s", f.label(), f.Outside)
	}
	return f.label()
}

// refusalNote is the prompt that continues a turn the harness cancelled after refusals. Nothing
// the refused actions would have done happened.
func refusalNote(refs []refusal, cwd string) string {
	var b strings.Builder
	b.WriteString("[Koinon] Your last action was refused, so the turn stopped. Nothing it asked for was done:\n")
	for _, f := range refs {
		switch {
		case f.Outside != "":
			fmt.Fprintf(&b, "- %s: %s is outside your session directory %s. You can read and change files only under it.\n",
				f.label(), f.Outside, cwd)
		default:
			fmt.Fprintf(&b, "- %s: Koinon does not allow this kind of action.\n", f.label())
		}
	}
	fmt.Fprintf(&b, "Use a path under %s (for notes, %s/notes/) or another way to do it, and continue the task "+
		"from where you stopped. Do not repeat the refused action.", cwd, strings.TrimSuffix(cwd, "/"))
	return b.String()
}

// noteRefusal remembers a rejected request for the turn in progress.
func (r *runner) noteRefusal(req acp.PermissionRequest, d decision) {
	f := refusal{Kind: d.Kind, Outside: d.Outside}
	if req.ToolCall.Title != nil {
		f.Title = *req.ToolCall.Title
	}
	r.refMu.Lock()
	defer r.refMu.Unlock()
	r.refusals = append(r.refusals, f)
}

// takeRefusals returns what agentd refused since the last call, and forgets it.
func (r *runner) takeRefusals() []refusal {
	r.refMu.Lock()
	defer r.refMu.Unlock()
	out := r.refusals
	r.refusals = nil
	return out
}
