// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcptoolpin"
)

// The tool list is reviewed configuration, not session data (#1132, ADR 0097 step 2). The text of a
// tool reaches the model as instructions, so the gateway serves the text an admin approved and nothing
// a server published since. A tool whose sanitised text hashes to another value than the approved
// one is left out of the list, and a call to it is refused, until an admin approves it again.

// ToolList is the tools/list result of one target for one agent, as the target sent it.
type ToolList struct {
	Agent      gateway.AgentRef
	Target     string
	Org        string
	Credential string
	// Tools are the tools the target listed. The gateway has cut the list to mcptoolpin.MaxTools.
	Tools []mcptoolpin.Published
}

// ToolListVerdict is the guard's decision on one ToolList.
type ToolListVerdict struct {
	// Serve are the tools the agent may see, with the text it is shown. The gateway shows exactly this.
	Serve []mcptoolpin.Tool
	// Passthrough says the list is Koinon's own (the prober, an own target) and goes on as the target
	// sent it. Serve is ignored then.
	Passthrough bool
	// Left are the names of the tools left out because their text is not the approved text. A call to
	// one is refused.
	Left []string
	// Waiting are the names of the tools left out because no admin reviewed them yet (#1216). A call to
	// one is refused.
	Waiting []string
	// Refuse withholds the whole list; Reason is fixed text.
	Refuse bool
	Reason string
}

// ToolListChecker is the optional part of a Guard that decides on a tool list. A guard with none
// leaves the list as it is, apart from the count limit the gateway puts on it.
type ToolListChecker interface {
	CheckToolList(ctx context.Context, l ToolList) ToolListVerdict
}

const reasonToolListUnreadable = "refused: the gateway could not check the tool list before this call"

// toolPinTTL is how long a (target, agent) list check holds for the calls of one replica. A call after
// it checks the list again first, so a replica that never listed (another replica served the list, a
// restart) still refuses a changed tool.
const toolPinTTL = 30 * time.Second

const reasonToolChanged = "refused: the tool changed since an admin approved it; an admin must review it again"

// reasonToolUnreviewed refuses a call to a tool that no admin reviewed (#1216). It is fixed text: the
// agent does not wait for the review.
const reasonToolUnreviewed = "refused: no admin reviewed this tool yet; it is not available until an admin reviews it"

// checkToolList runs the list check, if the guard has one. A guard that panics withholds the list.
func (f failClosed) checkToolList(ctx context.Context, l ToolList) (v ToolListVerdict) {
	if !f.loaded() {
		return ToolListVerdict{Refuse: true, Reason: reasonGuardNotLoaded}
	}
	lc, ok := f.g.(ToolListChecker)
	if !ok {
		return ToolListVerdict{Passthrough: true}
	}
	defer func() {
		if recover() != nil {
			f.log.ErrorContext(ctx, "guard panicked", "target", l.Target, "kind", "tool_list")
			f.m.guardFailure(guardPanic)
			v = ToolListVerdict{Refuse: true, Reason: reasonGuardFailed}
		}
	}()
	return lc.CheckToolList(ctx, l)
}

// Fixed reasons a tool is left out of a list: metric labels (ADR 0023).
const (
	leftChanged    = "changed"
	leftOverLimit  = "over_limit"
	leftUnusable   = "unusable"
	leftUnreadable = "unreadable"
	leftUnreviewed = "unreviewed"
)

// leftOutSet remembers the tools each (target, agent) list left out, with the reason a call to each
// is refused, so a call to one is refused without a call to the target. It is updated by every list
// and by a list_changed notification.
type leftOutSet struct {
	mu sync.Mutex
	m  map[poolKey]map[string]string
	at map[poolKey]time.Time
}

// set records the tools one list left out: changed names the tools whose text is not the approved
// text (or that the gateway could not check), waiting the tools no admin reviewed yet.
func (s *leftOutSet) set(k poolKey, changed, waiting []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[poolKey]time.Time{}
	}
	s.at[k] = time.Now()
	if len(changed)+len(waiting) == 0 {
		delete(s.m, k)
		return
	}
	if s.m == nil {
		s.m = map[poolKey]map[string]string{}
	}
	// No size hint: the lists come from a server, and a sum of two lengths is not a safe hint.
	set := map[string]string{}
	for _, n := range waiting {
		set[n] = reasonToolUnreviewed
	}
	// A name in both lists (a server that lists one name twice) takes the changed reason.
	for _, n := range changed {
		set[n] = reasonToolChanged
	}
	s.m[k] = set
}

// fresh reports whether a list check of k finished within ttl.
func (s *leftOutSet) fresh(k poolKey, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[k]
	return ok && time.Since(t) < ttl
}

// refusal returns the fixed reason a call to a tool that a list left out is refused, and false for a
// tool the list served.
func (s *leftOutSet) refusal(k poolKey, tool string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[k][tool]
	return r, ok
}

func (s *leftOutSet) forget(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if k.target == target {
			delete(s.m, k)
		}
	}
	for k := range s.at {
		if k.target == target {
			delete(s.at, k)
		}
	}
}

// published reads the members of a tool that the model sees. A tool that cannot be encoded is
// reported with ok false: it is left out.
func published(t *mcp.Tool) (mcptoolpin.Published, bool) {
	p := mcptoolpin.Published{Name: t.Name, Title: t.Title, Description: t.Description}
	var err error
	if t.Annotations != nil {
		if p.Annotations, err = json.Marshal(t.Annotations); err != nil {
			return p, false
		}
	}
	if t.InputSchema != nil {
		if p.InputSchema, err = json.Marshal(t.InputSchema); err != nil {
			return p, false
		}
	}
	if t.OutputSchema != nil {
		if p.OutputSchema, err = json.Marshal(t.OutputSchema); err != nil {
			return p, false
		}
	}
	return p, len(p.InputSchema)+len(p.OutputSchema)+len(p.Description) <= mcptoolpin.MaxToolBytes
}

// served builds the tool an agent sees from approved or sanitised text. The members are the ones the
// hash covers, and no others: _meta, icons and any member a server invented are not served.
func served(t mcptoolpin.Tool) (*mcp.Tool, bool) {
	var out mcp.Tool
	if err := json.Unmarshal(t.JSON(), &out); err != nil {
		return nil, false
	}
	return &out, true
}

// vetted applies the count limit and the guard's check to one target's list. It returns the tools to
// serve (still under the target's own names) and the names it left out. A false ok means the whole
// list is withheld.
func (g *Gateway) vetted(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef, got []*mcp.Tool) ([]*mcp.Tool, []string, bool) {
	key := poolKey{target: t.Name, agent: agent}
	// A list is cut in a fixed order, so the same list gives the same tools. A tool past the limit is
	// left out, never passed unchecked.
	slices.SortStableFunc(got, func(a, b *mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	var left []string
	if len(got) > mcptoolpin.MaxTools {
		for _, x := range got[mcptoolpin.MaxTools:] {
			left = append(left, x.Name)
			g.o.Metrics.toolLeft(leftOverLimit)
		}
		got = got[:mcptoolpin.MaxTools]
	}
	l := ToolList{Agent: agent, Target: t.Name, Org: t.Org, Tools: make([]mcptoolpin.Published, 0, len(got))}
	byName := make(map[string]*mcp.Tool, len(got))
	for _, x := range got {
		p, ok := published(x)
		if !ok {
			left = append(left, x.Name)
			g.o.Metrics.toolLeft(leftUnusable)
			continue
		}
		l.Tools = append(l.Tools, p)
		byName[x.Name] = x
	}
	if c, err := g.creds.Credential(ctx, t, agent); err == nil {
		l.Credential = string(c.Kind)
	} else {
		l.Credential = string(CredentialError)
	}
	v := g.g.checkToolList(ctx, l)
	switch {
	case v.Refuse:
		g.o.Metrics.toolLeft(leftUnreadable)
		g.o.Logger.WarnContext(scrubCtx(ctx, agent), "tools/list: list withheld", "target", t.Name)
		// The tools an earlier list left out stay out, and the check is no longer fresh: a call checks again.
		return nil, nil, false
	case v.Passthrough:
		g.left.set(key, left, nil)
		out := make([]*mcp.Tool, 0, len(l.Tools))
		for _, p := range l.Tools {
			out = append(out, byName[p.Name])
		}
		return out, left, true
	}
	out := make([]*mcp.Tool, 0, len(v.Serve))
	for _, s := range v.Serve {
		// Only a tool the target listed can be served: the guard cannot add one.
		if _, listed := byName[s.Name]; !listed {
			continue
		}
		if m, ok := served(s); ok {
			out = append(out, m)
		}
	}
	for _, n := range v.Left {
		g.o.Metrics.toolLeft(leftChanged)
		g.o.Logger.WarnContext(scrubCtx(ctx, agent), "tools/list: tool left out; it changed since it was approved",
			"target", t.Name, "tool", logName(g.o.Scrub.TextFor(agent.Org, n)))
	}
	for range v.Waiting {
		// No log line for each tool: a server can list many new names, and the name is its own text.
		g.o.Metrics.toolLeft(leftUnreviewed)
	}
	left = append(left, v.Left...)
	g.left.set(key, left, v.Waiting)
	return out, append(left, v.Waiting...), true
}

// refresh lists a target again for one agent and updates the tools left out. It is what a
// notifications/tools/list_changed from the target runs: the same check as a tools/list.
func (g *Gateway) refresh(t gateway.MCPTarget, agent gateway.AgentRef) {
	key := poolKey{target: t.Name, agent: agent}
	g.mu.Lock()
	if g.drain || g.refreshing[key] {
		g.mu.Unlock()
		return
	}
	if g.refreshing == nil {
		g.refreshing = map[poolKey]bool{}
	}
	g.refreshing[key] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.refreshing, key)
		g.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(WithScrubOrg(context.Background(), agent.Org), g.o.ListTimeout)
	defer cancel()
	got, err := g.targetTools(ctx, t, agent)
	if err != nil {
		g.o.Logger.WarnContext(ctx, "tools/list_changed: relist failed", "target", t.Name, "error", errKind(err))
		return
	}
	g.vetted(ctx, t, agent, got)
}

// logName cuts a server's tool name for a log line. The caller scrubs the name first: a cut could split a
// credential value, and the scrub matches whole values only.
func logName(n string) string {
	if len(n) > 128 {
		return n[:128]
	}
	return n
}

// checksToolLists reports whether the guard decides on tool lists.
func (f failClosed) checksToolLists() bool {
	_, ok := f.g.(ToolListChecker)
	return ok && f.loaded()
}

// recheck lists the target for the agent and runs the check before a call, when no check of this
// replica is fresh. It returns false when the list could not be checked: the call is refused.
func (g *Gateway) recheck(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) bool {
	lctx, cancel := context.WithTimeout(ctx, g.o.ListTimeout)
	defer cancel()
	got, err := g.targetTools(lctx, t, agent)
	if err != nil {
		return false
	}
	_, _, ok := g.vetted(lctx, t, agent, got)
	return ok
}
