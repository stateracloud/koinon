// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// A tool's declaration (ADR 0091 decision 2) says its sink class, which arguments are address
// arguments and what its results carry for the session legs. A tool nobody declared is
// sinkclass.Default: external-high with the server's legs. The declaration goes back to that default
// when the server's URL changes (the old issue 350 rule, ReasonURLChanged) or when the tool's content hash
// changes (the ADR 0057 rule, ReasonContentChanged): it described the old tool.
const (
	// ReasonURLChanged is the reason of a reset caused by a change of the server's URL.
	ReasonURLChanged = "url_changed"
	// ReasonContentChanged is the reason of a reset caused by a change of the tool's content hash.
	ReasonContentChanged = "content_changed"
)

func sinkOf(t store.McpTool) sinkclass.Declaration {
	return sinkclass.Declaration{
		Class: sinkclass.Class(t.SinkClass), Derivation: sinkclass.Derivation(t.SinkDerivation),
		AddressArgs:          append([]string{}, t.AddressArgs...),
		ResultUntrustedInput: boolOf(t.ResultUntrustedInput), ResultPrivateData: boolOf(t.ResultPrivateData),
	}
}

func boolOf(b pgtype.Bool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

func pgBool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func sinkSummary(server, tool string, d sinkclass.Declaration) map[string]any {
	m := map[string]any{
		"server": server, "tool": tool, "sinkClass": string(d.Class), "derivation": string(d.Derivation), "addressArgs": d.AddressArgs,
	}
	if d.ResultUntrustedInput != nil {
		m["resultUntrustedInput"] = *d.ResultUntrustedInput
	}
	if d.ResultPrivateData != nil {
		m["resultPrivateData"] = *d.ResultPrivateData
	}
	return m
}

// SetToolDeclaration sets a tool's declaration. It works for every source of server. Audited as
// mcp_tool.declaration.set with the declaration before and after.
//
// expectedHash is the content hash of the tool the admin reviewed (Tool.ContentHash). The tool is read
// under a row lock, and a different hash is ErrToolChanged: the declaration described text that is
// gone, which is the case the content-hash reset exists for. An empty hash is invalid.
func (s *Service) SetToolDeclaration(ctx context.Context, org pgtype.UUID, actor audit.Actor, serverID, tool, expectedHash string, d sinkclass.Declaration) (Tool, error) {
	sid, err := parseID(serverID)
	if err != nil {
		return Tool{}, err
	}
	d, err = d.Normalize()
	if err != nil {
		return Tool{}, mapSinkErr(err)
	}
	if expectedHash == "" {
		return Tool{}, invalid("contentHash is required: the hash of the tool you reviewed")
	}
	var out store.McpTool
	err = s.tx(ctx, func(q *store.Queries) error {
		srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: sid})
		if err != nil {
			return err
		}
		cur, err := q.GetMCPToolForUpdate(ctx, store.GetMCPToolForUpdateParams{OrgID: org, ServerID: sid, Name: tool})
		if err != nil {
			return err
		}
		if cur.ContentHash != expectedHash {
			return ErrToolChanged
		}
		out, err = q.SetMCPToolDeclaration(ctx, store.SetMCPToolDeclarationParams{
			OrgID: org, ServerID: sid, Name: tool, SinkClass: store.McpSinkClass(d.Class), SinkDerivation: store.McpSinkDerivation(d.Derivation),
			AddressArgs: d.AddressArgs, ResultUntrustedInput: pgBool(d.ResultUntrustedInput), ResultPrivateData: pgBool(d.ResultPrivateData),
		})
		if err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "mcp_tool.declaration.set", TargetKind: "mcp_tool", TargetID: serverID + "/" + tool,
			Before: sinkSummary(srv.Name, tool, sinkOf(cur)), After: sinkSummary(srv.Name, tool, sinkOf(out)),
		})
	})
	if err != nil {
		return Tool{}, mapErr(err)
	}
	return toTool(out), nil
}

func mapSinkErr(err error) error {
	var ie *sinkclass.InvalidError
	if errors.As(err, &ie) {
		return &InvalidError{Msg: ie.Msg}
	}
	return err
}

// recordToolDeclarationReset audits the reset a content change made to one tool's declaration (the
// UpdateMCPToolContent query does the reset itself). It records nothing when the declaration was the
// default already. The event is mcp_tool.declaration.reset with reason content_changed.
func recordToolDeclarationReset(ctx context.Context, q *store.Queries, org pgtype.UUID, actor audit.Actor, server string, cur, out store.McpTool, reason string) error {
	before, after := sinkOf(cur), sinkOf(out)
	if before.Equal(after) {
		return nil
	}
	a := sinkSummary(server, out.Name, after)
	a["reason"] = reason
	return audit.RecordErr(ctx, q, audit.Event{
		Org: org, Actor: actor, Action: "mcp_tool.declaration.reset", TargetKind: "mcp_tool", TargetID: out.ServerID.String() + "/" + out.Name,
		Before: sinkSummary(server, out.Name, before), After: a,
	})
}

// resetOnURLChange is what a changed URL does besides the update itself: the server's data class and
// every tool's declaration go back to the default, each audited. It does nothing when the URL is the same.
func resetOnURLChange(ctx context.Context, q *store.Queries, org pgtype.UUID, actor audit.Actor, cur, out store.McpServer) error {
	if cur.Url == out.Url {
		return nil
	}
	if err := recordClassReset(ctx, q, org, actor, cur, out); err != nil {
		return err
	}
	rows, err := q.ResetMCPToolDeclarations(ctx, store.ResetMCPToolDeclarationsParams{OrgID: org, ServerID: out.ID})
	if err != nil {
		return err
	}
	def := sinkclass.Default()
	for _, r := range rows {
		before := sinkclass.Declaration{
			Class: sinkclass.Class(r.OldSinkClass), Derivation: sinkclass.Derivation(r.OldSinkDerivation),
			AddressArgs:          append([]string{}, r.OldAddressArgs...),
			ResultUntrustedInput: boolOf(r.OldResultUntrustedInput), ResultPrivateData: boolOf(r.OldResultPrivateData),
		}
		after := sinkSummary(out.Name, r.Name, def)
		after["reason"] = ReasonURLChanged
		if err := audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "mcp_tool.declaration.reset", TargetKind: "mcp_tool", TargetID: out.ID.String() + "/" + r.Name,
			Before: sinkSummary(cur.Name, r.Name, before), After: after,
		}); err != nil {
			return err
		}
	}
	return nil
}
