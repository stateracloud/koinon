// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

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
	// Refuse withholds the whole list; Reason is fixed text.
	Refuse bool
	Reason string
}

// ToolListChecker is the optional part of a Guard that decides on a tool list. A guard with none
// leaves the list as it is, apart from the count limit the gateway puts on it.
type ToolListChecker interface {
	CheckToolList(ctx context.Context, l ToolList) ToolListVerdict
}

const reasonToolChanged = "refused: the tool changed since an admin approved it; an admin must review it again"

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
			f.log.Error("guard panicked", "target", l.Target, "kind", "tool_list")
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
)

// leftOutSet remembers the tools each (target, agent) list left out, so a call to one is refused
// without a call to the target. It is updated by every list and by a list_changed notification.
type leftOutSet struct {
	mu sync.Mutex
	m  map[poolKey]map[string]struct{}
}

func (s *leftOutSet) set(k poolKey, names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(names) == 0 {
		delete(s.m, k)
		return
	}
	if s.m == nil {
		s.m = map[poolKey]map[string]struct{}{}
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	s.m[k] = set
}

func (s *leftOutSet) has(k poolKey, tool string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[k][tool]
	return ok
}

func (s *leftOutSet) forget(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if k.target == target {
			delete(s.m, k)
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
	if c, err := g.o.Credentials.Credential(ctx, t, agent); err == nil {
		l.Credential = c.Kind
	} else {
		l.Credential = CredentialError
	}
	v := g.g.checkToolList(ctx, l)
	switch {
	case v.Refuse:
		g.o.Metrics.toolLeft(leftUnreadable)
		g.o.Logger.Warn("tools/list: list withheld", "target", t.Name)
		g.left.set(key, nil)
		return nil, nil, false
	case v.Passthrough:
		g.left.set(key, left)
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
		g.o.Logger.Warn("tools/list: tool left out; it changed since it was approved", "target", t.Name, "tool", n)
	}
	left = append(left, v.Left...)
	g.left.set(key, left)
	return out, left, true
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
	ctx, cancel := context.WithTimeout(context.Background(), g.o.ListTimeout)
	defer cancel()
	got, err := g.targetTools(ctx, t, agent)
	if err != nil {
		g.o.Logger.Warn("tools/list_changed: relist failed", "target", t.Name, "error", errKind(err))
		return
	}
	g.vetted(ctx, t, agent, got)
}
