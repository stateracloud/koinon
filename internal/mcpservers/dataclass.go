// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The data class of a server (ADR 0084 decision 8, ADR 0091 decision 4) says which session legs its
// results can set. A server may carry both. Outbound is not a leg of the server: ADR 0091 makes it the
// sink class of each tool (SetToolDeclaration). The leak guard and the templates test read the
// declaration instead of guessing from names.
const (
	// LegUntrustedInput: the server returns text an outsider controls (web pages, issues, mail).
	LegUntrustedInput = "untrusted_input"
	// LegPrivateData: the server returns data the org does not want to leave (repositories, internal
	// documents, FinOps data).
	LegPrivateData = "private_data"
)

// Kinds of server. KindOther is the default and says nothing about the server.
const (
	KindFetch  = "fetch"
	KindSearch = "search"
	KindOther  = "other"
)

// Legs are the two legs in their canonical order, the order Server.DataClass lists them in.
var Legs = []string{LegUntrustedInput, LegPrivateData}

// Kinds are the kinds a server can declare.
var Kinds = []string{KindFetch, KindSearch, KindOther}

// Declaration is what a server says it exposes.
type Declaration struct {
	// Kind is KindFetch, KindSearch or KindOther.
	Kind string
	// Legs are the legs the server carries, any subset of Legs; empty means none.
	Legs []string
	// CodeHostConnection is the ID of the org's code-host connection the server acts through, "" for
	// none. A code-host tool of a server without one is external-high (ADR 0091 decision 3). A change
	// of the server URL clears it.
	CodeHostConnection string
}

// DefaultDeclaration is what a server that nobody classified declares: kind other and both
// legs. The cautious reading, so a server is not exempt from the guard because nobody described it.
// It is also what migration 00050 gave the servers that existed.
func DefaultDeclaration() Declaration {
	return Declaration{Kind: KindOther, Legs: slices.Clone(Legs)}
}

// Normalize validates d and returns it with its legs deduplicated and in canonical order.
func (d Declaration) Normalize() (Declaration, error) {
	if !slices.Contains(Kinds, d.Kind) {
		return Declaration{}, invalid("kind must be one of %s", strings.Join(Kinds, ", "))
	}
	for _, l := range d.Legs {
		if !slices.Contains(Legs, l) {
			return Declaration{}, invalid("data class %q is not one of %s", l, strings.Join(Legs, ", "))
		}
	}
	out := Declaration{Kind: d.Kind, Legs: []string{}}
	if d.CodeHostConnection != "" {
		var id pgtype.UUID
		if err := id.Scan(d.CodeHostConnection); err != nil || !id.Valid {
			return Declaration{}, invalid("codeHostConnectionId %q is not a connection ID", d.CodeHostConnection)
		}
		out.CodeHostConnection = id.String()
	}
	for _, l := range Legs {
		if slices.Contains(d.Legs, l) {
			out.Legs = append(out.Legs, l)
		}
	}
	return out, nil
}

// Carries reports whether the server declares the leg.
func (s Server) Carries(leg string) bool { return slices.Contains(s.DataClass, leg) }

func declarationOf(r store.McpServer) Declaration {
	d := Declaration{Kind: string(r.Kind), Legs: []string{}}
	for _, l := range []struct {
		on  bool
		leg string
	}{{r.ExposesUntrustedInput, LegUntrustedInput}, {r.ExposesPrivateData, LegPrivateData}} {
		if l.on {
			d.Legs = append(d.Legs, l.leg)
		}
	}
	if r.CodeHostConnectionID.Valid {
		d.CodeHostConnection = r.CodeHostConnectionID.String()
	}
	return d
}

// connectionParam is the nullable UUID of a declaration's connection. Normalize checked its shape.
func connectionParam(d Declaration) pgtype.UUID {
	var id pgtype.UUID
	if d.CodeHostConnection != "" {
		_ = id.Scan(d.CodeHostConnection)
	}
	return id
}

// createClass fills the class columns of a new server: the declaration, or the column defaults.
func createClass(p *store.CreateMCPServerParams, d *Declaration) {
	if d == nil {
		return
	}
	p.Kind = store.NullMcpServerKind{McpServerKind: store.McpServerKind(d.Kind), Valid: true}
	p.ExposesUntrustedInput = pgtype.Bool{Bool: slices.Contains(d.Legs, LegUntrustedInput), Valid: true}
	p.ExposesPrivateData = pgtype.Bool{Bool: slices.Contains(d.Legs, LegPrivateData), Valid: true}
	p.CodeHostConnectionID = connectionParam(*d)
}

// SetDataClass sets what a server exposes. It works for every source: a declared server's Secret
// does not carry it, and the Secret's next apply leaves it alone. Audited as mcp_server.data_class.set.
func (s *Service) SetDataClass(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string, d Declaration) (Server, error) {
	sid, err := parseID(id)
	if err != nil {
		return Server{}, err
	}
	d, err = d.Normalize()
	if err != nil {
		return Server{}, err
	}
	var out store.McpServer
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		link, err := lockLinkTarget(ctx, q, org, d)
		if err != nil {
			return err
		}
		out, err = q.SetMCPServerDataClass(ctx, store.SetMCPServerDataClassParams{
			OrgID: org, ID: sid, Kind: store.McpServerKind(d.Kind),
			ExposesUntrustedInput: slices.Contains(d.Legs, LegUntrustedInput),
			ExposesPrivateData:    slices.Contains(d.Legs, LegPrivateData),
			CodeHostConnectionID:  connectionParam(d),
		})
		if err != nil {
			return err
		}
		// The URL of the row as written: a concurrent URL change committed since cur was read is
		// checked too (the update waited for its row lock).
		if err := link.check(out.Url); err != nil {
			return err
		}
		before, after := declarationOf(cur), declarationOf(out)
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_server.data_class.set", TargetKind: "mcp_server", TargetID: id,
			Before: map[string]any{"name": cur.Name, "kind": before.Kind, "dataClass": before.Legs, "codeHostConnectionId": before.CodeHostConnection},
			After:  map[string]any{"name": out.Name, "kind": after.Kind, "dataClass": after.Legs, "codeHostConnectionId": after.CodeHostConnection},
		})
		return nil
	})
	if err != nil {
		return Server{}, mapErr(err)
	}
	return toServer(out), nil
}

// recordClassReset audits the reset a URL change made to the data class (the UpdateMCPServer and
// UpdateDeclaredMCPServer queries do the reset itself). It records nothing when the class was already
// the default. The event is mcp_server.data_class.reset with reason url_changed.
func recordClassReset(ctx context.Context, q *store.Queries, b audit.Batch, org pgtype.UUID, actor audit.Actor, cur, out store.McpServer) error {
	if cur.Url == out.Url {
		return nil
	}
	before, after := declarationOf(cur), declarationOf(out)
	if before.Kind == after.Kind && slices.Equal(before.Legs, after.Legs) && before.CodeHostConnection == after.CodeHostConnection {
		return nil
	}
	b.Org(audit.Event{
		Org: org, Actor: actor, Action: "mcp_server.data_class.reset", TargetKind: "mcp_server", TargetID: out.ID.String(),
		Before: map[string]any{"name": cur.Name, "kind": before.Kind, "dataClass": before.Legs, "codeHostConnectionId": before.CodeHostConnection},
		After:  map[string]any{"name": out.Name, "kind": after.Kind, "dataClass": after.Legs, "codeHostConnectionId": after.CodeHostConnection, "reason": "url_changed"},
	})
	return nil
}
