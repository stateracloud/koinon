// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Declared is a server declared by a Kubernetes Secret of type mcp-server.
type Declared struct {
	// SecretName is the declaring Secret; it is the audit actor.
	SecretName string
	// Version identifies the Secret's content ("<uid>/<resourceVersion>"). Applying the same
	// version again changes nothing.
	Version          string
	Name             string
	URL              string
	Transport        string
	CredentialHeader string
	CredentialPrefix string
	CredentialValue  string
}

// ApplyDeclared creates or updates the server a Secret declares and reports whether anything
// changed. A declared server wins a name collision: the UI server of the same name is disabled
// until the declared one goes. A name declared by another Secret is an error.
func (s *Service) ApplyDeclared(ctx context.Context, org pgtype.UUID, d Declared) (changed bool, err error) {
	in := Input{Name: d.Name, URL: d.URL, Transport: d.Transport, CredentialHeader: d.CredentialHeader, CredentialPrefix: d.CredentialPrefix, CredentialValue: d.CredentialValue}
	normalize(&in)
	if err := ValidateName(in.Name); err != nil {
		return false, err
	}
	if err := ValidateRemote(in.URL, in.Transport, in.CredentialHeader, in.CredentialPrefix); err != nil {
		return false, err
	}
	if err := s.policy.Check(ctx, in.URL); err != nil {
		return false, err
	}
	actor := audit.Secret(d.SecretName)
	err = s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		all, err := q.ListMCPServers(ctx, org)
		if err != nil {
			return err
		}
		var cur *store.McpServer
		for i, r := range all {
			if r.Source != store.ConfigSourceSecret {
				continue
			}
			switch {
			case r.Name == d.Name && r.ManagedBySecret.String != d.SecretName:
				return invalid("MCP server %q is already declared by the Secret %s", d.Name, r.ManagedBySecret.String)
			case r.Name == d.Name:
				cur = &all[i]
			case r.ManagedBySecret.String == d.SecretName:
				// The Secret renamed its server: the old entry goes.
				if err := s.removeServer(ctx, q, b, org, r); err != nil {
					return err
				}
			}
		}
		// A lost credential (keys forget) is written again from the Secret, which still holds it.
		if cur != nil && cur.DeclaredVersion.String == d.Version && !cur.RemovedAt.Valid && !cur.CredentialLostAt.Valid {
			return nil
		}
		// A new server or a new address is one the prober will dial: a held install applies neither
		// (the importer skips this Secret and goes on; ADR 0107). A change of credential alone dials nothing.
		if cur == nil || cur.Url != in.URL || string(cur.Transport) != in.Transport {
			if err := s.hold.Err(); err != nil {
				return err
			}
		}
		// A credential-less probe of the new settings is not run here: a declared server may be
		// applied before the server it names is reachable. The prober reports its health.
		if err := q.ShadowUIMCPServers(ctx, store.ShadowUIMCPServersParams{OrgID: org, Name: d.Name, Reason: pgtype.Text{String: ReasonShadowed, Valid: true}}); err != nil {
			return err
		}
		var id pgtype.UUID
		var row store.McpServer
		var before map[string]any
		if cur == nil {
			if id, err = newID(); err != nil {
				return err
			}
			cred, err := s.seal(org, id, in.CredentialValue)
			if err != nil {
				return err
			}
			row, err = q.CreateMCPServer(ctx, store.CreateMCPServerParams{
				OrgID: org, ID: id, Name: in.Name, Url: in.URL, Transport: store.McpTransport(in.Transport),
				CredentialHeader: pgtype.Text{String: in.CredentialHeader, Valid: len(cred) > 0}, CredentialPrefix: prefixIf(in.CredentialPrefix, len(cred) > 0), Credential: cred,
				Source: store.ConfigSourceSecret, ManagedBySecret: pgtype.Text{String: d.SecretName, Valid: true},
				DeclaredVersion: pgtype.Text{String: d.Version, Valid: true},
			})
			if err != nil {
				return err
			}
		} else {
			id, before = cur.ID, serverSummary(*cur)
			cred, err := s.seal(org, id, in.CredentialValue)
			if err != nil {
				return err
			}
			row, err = q.UpdateDeclaredMCPServer(ctx, store.UpdateDeclaredMCPServerParams{
				OrgID: org, ID: id, Url: in.URL, Transport: store.McpTransport(in.Transport),
				CredentialHeader: pgtype.Text{String: in.CredentialHeader, Valid: len(cred) > 0}, CredentialPrefix: prefixIf(in.CredentialPrefix, len(cred) > 0), Credential: cred,
				DeclaredVersion: pgtype.Text{String: d.Version, Valid: true},
			})
			if err != nil {
				return err
			}
		}
		if err := q.ReenableUIMCPServers(ctx, org); err != nil {
			return err
		}
		changed = true
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "mcp_server.declare", TargetKind: "mcp_server", TargetID: id.String(),
			Before: before, After: serverSummary(row),
		})
		if cur != nil {
			return resetOnURLChange(ctx, q, b, org, actor, *cur, row)
		}
		return nil
	})
	if err != nil {
		return false, mapErr(err)
	}
	return changed, nil
}

// removeServer marks a declared server removed: it stays as a disabled row, so the audit trail and
// the tiers an admin set keep a target, and the Secret coming back restores it.
func (s *Service) removeServer(ctx context.Context, q *store.Queries, b audit.Batch, org pgtype.UUID, r store.McpServer) error {
	if r.RemovedAt.Valid {
		return nil
	}
	if err := q.MarkMCPServerRemoved(ctx, store.MarkMCPServerRemovedParams{OrgID: org, ID: r.ID, Reason: pgtype.Text{String: ReasonRemoved, Valid: true}}); err != nil {
		return err
	}
	b.Org(audit.Event{
		Org: org, Actor: audit.Secret(r.ManagedBySecret.String), Action: "mcp_server.undeclare", TargetKind: "mcp_server",
		TargetID: r.ID.String(), Before: serverSummary(r), After: map[string]any{"removed": true},
	})
	return nil
}

// DeclaredSecrets returns the names of the Secrets that declare servers in org.
func (s *Service) DeclaredSecrets(ctx context.Context, org pgtype.UUID) ([]string, error) {
	rows, err := store.New(s.pool).ListMCPServers(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.Source == store.ConfigSourceSecret && !r.RemovedAt.Valid && !slices.Contains(out, r.ManagedBySecret.String) {
			out = append(out, r.ManagedBySecret.String)
		}
	}
	return out, nil
}

// RemoveDeclared marks what the Secret declared in org removed (its Secret was deleted), and
// re-enables the UI servers it shadowed.
func (s *Service) RemoveDeclared(ctx context.Context, org pgtype.UUID, secretName string) error {
	return mapErr(s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		all, err := q.ListMCPServers(ctx, org)
		if err != nil {
			return err
		}
		for _, r := range all {
			if r.Source == store.ConfigSourceSecret && r.ManagedBySecret.String == secretName {
				if err := s.removeServer(ctx, q, b, org, r); err != nil {
					return err
				}
			}
		}
		return q.ReenableUIMCPServers(ctx, org)
	}))
}
