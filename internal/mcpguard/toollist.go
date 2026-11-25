// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
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
//   - A tool no admin approved (new, or in the ask tier of a first sync) is hidden: it is not served, and
//     a call to it is refused (#1216, ADR 0125). When the sync has not recorded that text, the guard
//     wakes the prober too, so the tool reaches review now and not at the next probe interval.
//   - On a server where an admin turned on "show new tools before review", such a tool is served as
//     sanitised text instead. It sets the untrusted-input leg, and only when the server declares that
//     leg: the leg alone never causes an approval.
//   - The guard never fingerprints tool text: it is reviewed configuration, not session data.
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
	if c.ask {
		v, unreviewed := askToolList(l, rows, c.server.ShowUnreviewedTools)
		if unreviewed && c.server.ExposesUntrustedInput {
			legs := Legs{UntrustedInput: true}
			if _, err := a.g.record(ctx, c, legs, nil); err != nil {
				return a.unreadableList(ctx, agent, l.Target, err)
			}
			a.g.metrics.legs(legs)
		}
		return v
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
			// A tool the sync never recorded (new since the last probe), or whose unreviewed text moved
			// since: the console cannot show it for review until the prober syncs the server.
			unsynced = unsynced || recorded[p.Name] != t.Hash()
			if !c.server.ShowUnreviewedTools {
				// No admin reviewed the tool: it is hidden until one does (#1216, ADR 0125), and a call
				// to it is refused.
				v.Waiting = append(v.Waiting, p.Name)
				continue
			}
			// An admin lets this server show new tools before the review: sanitised text.
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
		// Ask the prober to sync the server now instead of at its next interval (#1173). A changed tool
		// stays left out until an admin approves the new text; the wake changes only when the admin can
		// see it.
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

// askToolList decides the list of a server's ask target: the approval proxy (package mcpask), which
// offers the server's ask-tier tools. Each tool there has Koinon's fixed note before the server's text
// (mcpservers.AskToolNote), so its text never hashes to the approved hash. The guard serves the approved
// text of the server's row with the note, and so the model sees only approved text, as on the server's
// own target. A tool that is not in the ask tier, not present, not reviewed or changed since the review
// is left out, as the proxy does (#1216). On a server where an admin lets new tools show before the
// review, an unreviewed tool is served as the proxy listed it, sanitised, and unreviewed is true: the
// caller sets the untrusted-input leg, as on the server's own target. The status tool is Koinon's own
// and is served as listed. The proxy also checks the agent's allowance and the tier on every call.
func askToolList(l mcpgateway.ToolList, rows []store.McpTool, showUnreviewed bool) (v mcpgateway.ToolListVerdict, unreviewed bool) {
	byName := make(map[string]store.McpTool, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	for _, p := range l.Tools {
		if p.Name == mcpservers.AskStatusTool {
			t, _, err := mcptoolpin.Normalise(p)
			if err != nil {
				v.Left = append(v.Left, p.Name)
				continue
			}
			v.Serve = append(v.Serve, t)
			continue
		}
		r, ok := byName[p.Name]
		switch {
		case !ok || !r.Present || r.Tier != store.McpTierAsk:
			v.Left = append(v.Left, p.Name)
			continue
		case mcpservers.Unreviewed(r) && showUnreviewed:
			t, _, err := mcptoolpin.Normalise(p)
			if err != nil {
				v.Left = append(v.Left, p.Name)
				continue
			}
			unreviewed = true
			v.Serve = append(v.Serve, t)
			continue
		case mcpservers.Unreviewed(r):
			v.Waiting = append(v.Waiting, p.Name)
			continue
		case mcpservers.PendingChange(r):
			v.Left = append(v.Left, p.Name)
			continue
		}
		pt, err := mcptoolpin.Parse(r.ApprovedJson)
		if err != nil || pt.Hash() != r.ApprovedHash.String {
			// The stored text is not the text that was approved: nothing of it is served.
			v.Left = append(v.Left, p.Name)
			continue
		}
		pt.Description = mcpservers.AskToolNote + pt.Description
		v.Serve = append(v.Serve, pt)
	}
	return v, unreviewed
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
