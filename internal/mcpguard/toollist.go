// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcptoolpin"
)

var _ mcpgateway.ToolListChecker = (*GatewayGuard)(nil)

// Reasons the guard gives for a tool list or a call to a tool that changed. Fixed text (no tool text).
const reasonToolListUnreadable = "withheld by the leak guard: it could not read the tool list; try again later"

// CheckToolList decides what an agent sees of one target's tool list (#1132, ADR 0097 step 2, ADR 0091
// decision 4). A tool list is reviewed configuration: the guard compares the hash of each tool's
// sanitised text with the hash an admin approved.
//
//   - A tool with an approved hash and the same hash is served with the approved text, which sets no leg.
//   - A tool with an approved hash and another hash changed: it is left out, and a call to it is refused.
//     The sync records the change for review (mcpservers.SyncTools); the guard writes nothing. When the
//     sync has not recorded that text yet, the guard wakes the prober to sync the server now (#1173).
//   - A tool no admin approved (new, or in the ask tier of a first sync) is served as sanitised text. It
//     sets the untrusted-input leg, and only when the server declares that leg: the leg alone never
//     causes an approval.
//   - A list the guard cannot record or read keeps both legs and is withheld (fail closed).
//
// Koinon's executor, the prober and Koinon's own targets are not an agent's session: their list goes on
// as it is. The prober needs the published text, which is what it hashes.
func (a *GatewayGuard) CheckToolList(ctx context.Context, l mcpgateway.ToolList) mcpgateway.ToolListVerdict {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	agent := l.Agent.String()
	c, refused, err := a.g.identify(ctx, agent, l.Target)
	if err != nil {
		return a.unreadableList(ctx, agent, l.Target, err)
	}
	if refused != nil {
		return mcpgateway.ToolListVerdict{Refuse: true, Reason: refused.Text}
	}
	if c.exempt || c.own {
		return mcpgateway.ToolListVerdict{Passthrough: true}
	}
	rows, err := a.g.q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: c.org.ID, ServerID: c.server.ID})
	if err != nil {
		return a.unreadableList(ctx, agent, l.Target, err)
	}
	approved := make(map[string]string, len(rows))
	stored := make(map[string]json.RawMessage, len(rows))
	recorded := make(map[string]string, len(rows))
	for _, r := range rows {
		recorded[r.Name] = r.ToolHash.String
		if r.ApprovedHash.Valid {
			approved[r.Name] = r.ApprovedHash.String
			stored[r.Name] = r.ApprovedJson
		}
	}
	var (
		v          mcpgateway.ToolListVerdict
		unreviewed bool
		unsynced   bool
	)
	for _, p := range l.Tools {
		t, _, err := mcptoolpin.Normalise(p)
		if err != nil {
			v.Left = append(v.Left, p.Name)
			continue
		}
		want, pinned := approved[p.Name]
		switch {
		case !pinned:
			unreviewed = true
			v.Serve = append(v.Serve, t)
		case want == t.Hash():
			pt, err := mcptoolpin.Parse(stored[p.Name])
			if err != nil || pt.Hash() != want {
				// The stored text is not the text that was approved: nothing of it is served.
				v.Left = append(v.Left, p.Name)
				continue
			}
			v.Serve = append(v.Serve, pt)
		default:
			v.Left = append(v.Left, p.Name)
			// The sync has not recorded this text yet, so no admin can see it to approve it.
			unsynced = unsynced || recorded[p.Name] != t.Hash()
		}
	}
	if unsynced {
		// Ask the prober to sync the server now instead of at its next interval (#1173). The tool stays
		// left out until an admin approves the new text; the wake changes only when the admin can see it.
		a.g.wakeProber(ctx, c.org.ID, c.server.ID)
	}
	if unreviewed && c.server.ExposesUntrustedInput {
		legs := Legs{UntrustedInput: true}
		if _, err := a.g.record(ctx, c, legs, nil); err != nil {
			return a.unreadableList(ctx, agent, l.Target, err)
		}
		a.g.metrics.legs(legs)
	}
	return v
}

// unreadableList withholds the list and keeps both legs on the session: the guard could not tell what
// the list brings into it (#254, fail closed). The refusal text is fixed.
func (a *GatewayGuard) unreadableList(ctx context.Context, agent, target string, cause error) mcpgateway.ToolListVerdict {
	var re refusedError
	if errors.As(cause, &re) {
		return mcpgateway.ToolListVerdict{Refuse: true, Reason: re.text}
	}
	a.log.Error("mcp guard: reading a tool list failed", "target", target, "err", cause)
	// Best effort: the list is withheld whatever this returns.
	if err := a.g.CheckUnseen(ctx, agent, target, nil); err != nil {
		a.log.Error("mcp guard: recording the legs of a tool list failed", "target", target, "err", err)
	}
	return mcpgateway.ToolListVerdict{Refuse: true, Reason: reasonToolListUnreadable}
}

// readableToolList reports whether a tools/list result has the shape of a list: an object whose
// "tools" member is an array. Anything else is a list the guard cannot read.
func readableToolList(result json.RawMessage) bool {
	var r struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(result, &r) == nil && r.Tools != nil
}
