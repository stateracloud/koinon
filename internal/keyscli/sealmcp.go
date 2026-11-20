// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// The install audit actions of the MCP credential move (ADR 0097 12.7): counts and key IDs, never a value.
const (
	ActionSealedV2 = "mcp_credential.sealed_v2"
	ActionSealedV1 = "mcp_credential.sealed_v1"
)

// ErrNoActiveGatewayKey means seal-mcp has no gateway key to seal to.
var ErrNoActiveGatewayKey = errors.New("no gateway recipient key is active: run the gateway key Job (init, then activate) first")

// ErrToV1Unavailable means this install cannot open a v2 credential from this process.
var ErrToV1Unavailable = errors.New("--to-v1 needs the private gateway key, which this identity cannot read: on a KMS or OpenBao install only koinon-gateway unwraps it, so a v2 credential cannot be moved back from here; set the credential again in the UI or API instead")

// SealMCPOptions chooses what seal-mcp does.
type SealMCPOptions struct {
	// Org limits the move to one org (a UUID). Empty means every org.
	Org string
	// DryRun opens every credential that would move and reports the counts. It writes nothing.
	DryRun bool
	// ToV1 is the reverse move: v2 to v1 under the active KEK.
	ToV1 bool
}

// SealMCPReport is what seal-mcp prints: counts only.
type SealMCPReport struct {
	Moved     int            // rows written
	WouldMove int            // dry run only
	Skipped   int            // rows another writer changed during the run, or already in the target format
	Failed    int            // rows that could not move
	Failures  map[string]int // by kind
	PerOrg    map[string]int // moved (or would move) rows per org
	KeyID     string         // the gateway key the rows are sealed to (to v2)
}

// SealMCP moves the stored MCP server credentials between envelope v1 (the KEK) and v2 (the gateway
// recipient key), ADR 0097 12.5 step 2. It is idempotent (a row already in the target format is not
// listed), resumable (every row is its own write), and org-scoped on request. Each write is a
// compare-and-swap on the envelope that was read, so a credential the API changes meanwhile is never
// overwritten: that row is counted as skipped and a rerun picks it up. The report holds counts and row
// IDs, never a value.
//
// To v2 it needs an active gateway key and, unless SealGate is nil, the write gate (every pod of every
// reader reports it reads v2). It never seals v2 while a pod cannot read it.
func (t *Tool) SealMCP(ctx context.Context, o SealMCPOptions) error {
	if o.Org != "" && !validUUID(o.Org) {
		return fmt.Errorf("--org %q is not a UUID", o.Org)
	}
	rep := SealMCPReport{Failures: map[string]int{}, PerOrg: map[string]int{}}
	var (
		ring    *secretstore.Keyring
		pub     secretstore.RecipientPublicKey
		openV2  *secretstore.RecipientKeyring
		activeK string
	)
	if o.ToV1 {
		if t.GatewayKeyring == nil {
			return ErrToV1Unavailable
		}
		var err error
		if openV2, err = t.GatewayKeyring(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrToV1Unavailable, err)
		}
		ks, err := t.Store.Load(ctx)
		if err != nil {
			return err
		}
		defer func() {
			for _, k := range ks.Keys {
				clear(k)
			}
		}()
		if ring, err = t.ring(ks); err != nil {
			return err
		}
		activeK = ks.Active
	} else {
		rows, err := gatewaykeys.List(ctx, t.Pool)
		if err != nil {
			return err
		}
		var act *gatewaykeys.Row
		for i := range rows {
			if rows[i].State == gatewaykeys.Active {
				act = &rows[i]
			}
		}
		if act == nil {
			return ErrNoActiveGatewayKey
		}
		if pub, err = act.PublicKey(); err != nil {
			return err
		}
		rep.KeyID = pub.ID()
		if t.SealGate != nil && !o.DryRun {
			if err := t.SealGate(ctx); err != nil {
				return fmt.Errorf("nothing was changed: %w", err)
			}
		}
		ks, err := t.Store.Load(ctx)
		if err != nil {
			return err
		}
		defer func() {
			for _, k := range ks.Keys {
				clear(k)
			}
		}()
		if ring, err = t.ring(ks); err != nil {
			return err
		}
		activeK = ks.Active
	}

	ids, err := t.mcpRows(ctx, o)
	if err != nil {
		return err
	}
	for _, r := range ids {
		moved, err := t.moveOne(ctx, o, r, ring, openV2, pub)
		switch {
		case err != nil:
			rep.Failed++
			rep.Failures[failureKind(err)]++
			_, _ = fmt.Fprintf(t.Out, "FAIL mcp_servers.credential org %s row %s: %s\n", r.org, r.id, failureKind(err))
		case moved == moveWrote:
			rep.Moved++
			rep.PerOrg[r.org]++
		case moved == moveDry:
			rep.WouldMove++
			rep.PerOrg[r.org]++
		default:
			rep.Skipped++
		}
	}
	t.printSealMCP(o, rep)
	var errs []error
	if rep.Failed > 0 {
		errs = append(errs, fmt.Errorf("%d credential(s) did not move; the rest did", rep.Failed))
	}
	if a := t.auditor(); a != nil && !o.DryRun && (rep.Moved > 0 || rep.Failed > 0) {
		action, to := ActionSealedV2, "v2"
		if o.ToV1 {
			action, to = ActionSealedV1, "v1"
		}
		after := map[string]any{"done": rep.Moved, "failed": rep.Failed, "skipped": rep.Skipped, "failures": rep.Failures, "format": to}
		if o.ToV1 {
			after["kek_id"] = activeK
		} else {
			after["recipient_key_id"] = rep.KeyID
		}
		if o.Org != "" {
			after["org"] = o.Org
		}
		if err := a.Record(ctx, Event{Command: "seal-mcp", Action: action, TargetKind: "mcp_credential", TargetID: "mcp_servers", After: after}); err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", ErrNotAudited, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Tool) printSealMCP(o SealMCPOptions, rep SealMCPReport) {
	to := "v2 (gateway key " + rep.KeyID + ")"
	if o.ToV1 {
		to = "v1 (KEK)"
	}
	if o.DryRun {
		_, _ = fmt.Fprintf(t.Out, "Dry run: %d MCP credential(s) would move to %s. Nothing was written.\n", rep.WouldMove, to)
	} else {
		_, _ = fmt.Fprintf(t.Out, "%d MCP credential(s) moved to %s; %d skipped (changed during the run, run it again); %d failed.\n", rep.Moved, to, rep.Skipped, rep.Failed)
	}
	orgs := make([]string, 0, len(rep.PerOrg))
	for org := range rep.PerOrg {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	for _, org := range orgs {
		_, _ = fmt.Fprintf(t.Out, "  org %s: %d\n", org, rep.PerOrg[org])
	}
}

type moveResult int

const (
	moveNone moveResult = iota
	moveWrote
	moveDry
)

// mcpRows lists the credentials that are not yet in the target format.
func (t *Tool) mcpRows(ctx context.Context, o SealMCPOptions) ([]rowRef, error) {
	cond := notV2("credential")
	if o.ToV1 {
		cond = "(credential->>'fmt') = '2'"
	}
	q := `SELECT org_id::text, id::text FROM mcp_servers WHERE credential IS NOT NULL AND ` + cond
	args := []any{}
	if o.Org != "" {
		q += ` AND org_id = $1`
		args = append(args, o.Org)
	}
	rows, err := t.Pool.Query(ctx, q+` ORDER BY org_id, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list the MCP credentials: %w", err)
	}
	defer rows.Close()
	var out []rowRef
	for rows.Next() {
		var r rowRef
		if err := rows.Scan(&r.org, &r.id); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// moveOne moves one credential. It reads the envelope, opens it, seals the new one and writes it only
// when the stored envelope is still the one it read.
func (t *Tool) moveOne(ctx context.Context, o SealMCPOptions, r rowRef, ring *secretstore.Keyring, openV2 *secretstore.RecipientKeyring, pub secretstore.RecipientPublicKey) (moveResult, error) {
	var old []byte
	err := t.Pool.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, r.org, r.id).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && old == nil) {
		return moveNone, nil // deleted or cleared meanwhile
	}
	if err != nil {
		return moveNone, err
	}
	f, ferr := secretstore.EnvelopeFormat(old)
	if ferr != nil {
		return moveNone, errUnreadable
	}
	if (f == secretstore.FormatV2) != o.ToV1 {
		return moveNone, nil // already in the target format
	}
	b := secretstore.Binding{OrgID: r.org, Table: mcpservers.ServersTable, RowID: r.id, Column: mcpservers.CredentialColumn}
	var plain []byte
	if o.ToV1 {
		var env secretstore.EnvelopeV2
		if err := json.Unmarshal(old, &env); err != nil {
			return moveNone, errUnreadable
		}
		if plain, err = openV2.Open(b, env); err != nil {
			return moveNone, err
		}
	} else {
		var env secretstore.Envelope
		if err := json.Unmarshal(old, &env); err != nil || secretstore.UnsupportedFormat(env) {
			return moveNone, errUnreadable
		}
		if plain, err = ring.Open(b, env); err != nil {
			return moveNone, err
		}
	}
	defer clear(plain)
	if o.DryRun {
		return moveDry, nil
	}
	var nb []byte
	if o.ToV1 {
		env, err := ring.Seal(b, plain)
		if err != nil {
			return moveNone, fmt.Errorf("%w: %w", errWrap, err)
		}
		nb, err = json.Marshal(env)
		if err != nil {
			return moveNone, err
		}
	} else {
		env, err := secretstore.SealV2(pub, b, plain)
		if err != nil {
			return moveNone, fmt.Errorf("%w: %w", errWrap, err)
		}
		if nb, err = json.Marshal(env); err != nil {
			return moveNone, err
		}
	}
	// The compare-and-swap: a write by the API between the read and here changes the stored envelope, and
	// this update then matches no row.
	tag, err := t.Pool.Exec(ctx, `UPDATE mcp_servers SET credential = $3 WHERE org_id = $1 AND id = $2 AND credential = $4::jsonb`, r.org, r.id, nb, bytes.TrimSpace(old))
	if err != nil {
		return moveNone, err
	}
	if tag.RowsAffected() != 1 {
		return moveNone, nil
	}
	return moveWrote, nil
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return false
		}
	}
	return true
}

// V2Usage counts the stored MCP credentials sealed to each gateway recipient key, by key ID, and the
// number still in v1. It reads the envelope header only.
func (t *Tool) V2Usage(ctx context.Context) (v2 map[string]int, v1 int, err error) {
	v2 = map[string]int{}
	rows, err := t.Pool.Query(ctx, `SELECT credential->>'recipient_key_id', (credential->>'fmt') = '2', count(*)
		FROM mcp_servers WHERE credential IS NOT NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id *string
		var isV2 *bool
		var n int
		if err := rows.Scan(&id, &isV2, &n); err != nil {
			return nil, 0, err
		}
		if isV2 != nil && *isV2 && id != nil {
			v2[*id] += n
		} else {
			v1 += n
		}
	}
	return v2, v1, rows.Err()
}

// mcpUsage prints the MCP credentials by format: how many are still v1 (sealed to a KEK) and how many are
// v2 under each gateway key ID. A table that does not exist yet (migration 00098) prints nothing.
func (t *Tool) mcpUsage(ctx context.Context) {
	v2, v1, err := t.V2Usage(ctx)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(t.Out, "\nMCP credentials: %d in v1 (sealed to a KEK)", v1)
	ids := make([]string, 0, len(v2))
	for id := range v2 {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		_, _ = fmt.Fprintf(t.Out, ", %d in v2 under gateway key %s", v2[id], id)
	}
	_, _ = fmt.Fprintln(t.Out, ".")
}
