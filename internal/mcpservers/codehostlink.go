// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The link of a server to a code-host connection (ADR 0091 decision 3) makes the server's code-host
// tools count a repository of that connection's list as inside. A server linked to the connection of
// another host would turn calls to that host into inside calls, so every write of a link checks that
// the server's URL names a host known to act on the connection's host (connectors.Type.ServesMCP,
// #117). The writes are Create and SetDataClass; a URL change clears the link (UpdateMCPServer,
// UpdateDeclaredMCPServer), and a change of a declared connection's host clears the links it no longer
// serves (UnlinkMismatchedServers).

// linkTarget is the connection a declaration links to, read and locked before the server row is
// written; nil when d links none.
type linkTarget struct{ conn *store.Connection }

// lockLinkTarget reads the connection d names and keeps it locked (FOR SHARE) until the transaction
// ends, so its host cannot change under the link. It takes the connection's lock before the
// server's, the order UnlinkMismatchedServers takes them in.
func lockLinkTarget(ctx context.Context, q *store.Queries, org pgtype.UUID, d Declaration) (linkTarget, error) {
	if d.CodeHostConnection == "" {
		return linkTarget{}, nil
	}
	conn, err := q.GetConnectionForMCPLink(ctx, store.GetConnectionForMCPLinkParams{OrgID: org, ID: connectionParam(d)})
	if errors.Is(err, pgx.ErrNoRows) {
		return linkTarget{}, invalid("codeHostConnectionId is not a connection of this org")
	}
	if err != nil {
		return linkTarget{}, err
	}
	return linkTarget{conn: &conn}, nil
}

// check refuses the link of a server at serverURL, the URL of the row as written.
func (l linkTarget) check(serverURL string) error {
	if l.conn == nil {
		return nil
	}
	if err := servesMCP(*l.conn, serverURL); err != nil {
		return invalid("codeHostConnectionId: %s", err.Error())
	}
	return nil
}

// servesMCP checks that a server at serverURL may act through conn. A connection that is not a code
// host, or whose host cannot be resolved, serves no server.
func servesMCP(conn store.Connection, serverURL string) error {
	t, ok := connectors.Get(conn.Connector)
	if !ok || t.Spec.Capabilities == nil {
		return fmt.Errorf("the connection %q is not a code-host connection", conn.DisplayName)
	}
	site, err := codehost.SiteOf(conn.Config)
	if err != nil {
		return fmt.Errorf("the connection %q has no readable host: %w", conn.DisplayName, err)
	}
	loc, err := site.Location(t)
	if err != nil {
		return fmt.Errorf("the connection %q has no readable host: %w", conn.DisplayName, err)
	}
	return t.ServesMCP(loc, conn.McpHosts, serverURL)
}

// MismatchedServers lists the servers of conn's org linked to conn that conn, as it is, does not
// serve: the servers UnlinkMismatchedServers would unlink. It writes nothing. The rows are locked
// (FOR UPDATE) until the transaction ends.
func MismatchedServers(ctx context.Context, q *store.Queries, conn store.Connection) ([]store.McpServer, error) {
	linked, err := q.ListMCPServersLinkedToConnectionForUpdate(ctx, store.ListMCPServersLinkedToConnectionForUpdateParams{OrgID: conn.OrgID, ConnectionID: conn.ID})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(linked, func(cur store.McpServer) bool { return servesMCP(conn, cur.Url) == nil }), nil
}

// UnlinkMismatchedServers clears the code-host link of every server of org linked to conn that conn,
// as it is now, does not serve: called in the transaction that changed the connection's hosts (a
// declared connection's baseUrl, or an admin's extra MCP host removed, #240), after the change. Each
// unlink is audited as mcp_server.data_class.reset with the given reason (connection_host_changed,
// connection_mcp_host_removed); the servers' kind and legs stay. It returns the servers it unlinked.
func UnlinkMismatchedServers(ctx context.Context, q *store.Queries, b audit.Batch, actor audit.Actor, conn store.Connection, reason string) ([]store.McpServer, error) {
	mismatched, err := MismatchedServers(ctx, q, conn)
	if err != nil {
		return nil, err
	}
	for _, cur := range mismatched {
		out, err := q.SetMCPServerDataClass(ctx, store.SetMCPServerDataClassParams{
			OrgID: cur.OrgID, ID: cur.ID, Kind: cur.Kind,
			ExposesUntrustedInput: cur.ExposesUntrustedInput, ExposesPrivateData: cur.ExposesPrivateData,
		})
		if err != nil {
			return nil, err
		}
		before, after := declarationOf(cur), declarationOf(out)
		b.Org(audit.Event{
			Org: cur.OrgID, Actor: actor, Action: "mcp_server.data_class.reset", TargetKind: "mcp_server", TargetID: cur.ID.String(),
			Before: map[string]any{"name": cur.Name, "kind": before.Kind, "dataClass": before.Legs, "codeHostConnectionId": before.CodeHostConnection},
			After: map[string]any{
				"name": out.Name, "kind": after.Kind, "dataClass": after.Legs, "codeHostConnectionId": after.CodeHostConnection,
				"reason": reason,
			},
		})
	}
	return mismatched, nil
}
