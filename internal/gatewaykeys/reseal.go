// SPDX-License-Identifier: Apache-2.0

package gatewaykeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpcredfile"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Resealer moves every stored v2 MCP credential that is sealed to an older gateway key to the active key
// (ADR 0097 12.4, spec 6). It runs under the gateway's identity: only the gateway holds the private keys.
// Each credential is its own write, a compare-and-swap on the envelope that was read, so a credential the
// API writes meanwhile is never lost: the row is skipped and a rerun moves it. The old key can retire once
// no credential names it.
type Resealer struct {
	DB store.DBTX
	// Keyring holds the private keys of the active key and every older key a credential still names.
	Keyring *secretstore.RecipientKeyring
	// Org limits the run to one org (a UUID); empty means every org.
	Org    string
	DryRun bool
}

// ResealReport is the result of a run: counts and key IDs, never a credential.
type ResealReport struct {
	KeyID     string         // the active key the credentials move to
	Moved     int            // rows written
	WouldMove int            // dry run only
	Skipped   int            // rows changed or removed by another writer during the run
	Failed    int            // rows that did not move
	Failures  map[string]int // by kind: unknown_key, open, format, database
	From      map[string]int // moved (or would move) rows by the key they left
}

// ErrNoActive is what Run returns with no active key to move to.
var ErrNoActive = errors.New("gatewaykeys: no gateway key is active, so there is nothing to re-seal to")

// Run re-seals. It returns the report and, when a row failed, an error after it did the rest.
func (r Resealer) Run(ctx context.Context) (ResealReport, error) {
	rep := ResealReport{Failures: map[string]int{}, From: map[string]int{}}
	rows, err := List(ctx, r.DB)
	if err != nil {
		return rep, err
	}
	var act *Row
	for i := range rows {
		if rows[i].State == Active {
			act = &rows[i]
		}
	}
	if act == nil {
		return rep, ErrNoActive
	}
	pub, err := act.PublicKey()
	if err != nil {
		return rep, err
	}
	rep.KeyID = pub.ID()
	if !r.Keyring.Has(pub.ID()) {
		return rep, fmt.Errorf("gatewaykeys: this process does not hold the private key of the active key %s", pub.ID())
	}
	q := `SELECT org_id::text, id::text FROM mcp_servers
		WHERE credential IS NOT NULL AND (credential->>'fmt') = '2' AND (credential->>'recipient_key_id') IS DISTINCT FROM $1`
	args := []any{pub.ID()}
	if r.Org != "" {
		q += ` AND org_id = $2`
		args = append(args, r.Org)
	}
	rs, err := r.DB.Query(ctx, q+` ORDER BY org_id, id`, args...)
	if err != nil {
		return rep, fmt.Errorf("gatewaykeys: list the credentials to re-seal: %w", err)
	}
	type ref struct{ org, id string }
	var refs []ref
	for rs.Next() {
		var x ref
		if err := rs.Scan(&x.org, &x.id); err != nil {
			rs.Close()
			return rep, err
		}
		refs = append(refs, x)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return rep, err
	}
	for _, x := range refs {
		from, moved, err := r.one(ctx, x.org, x.id, pub)
		switch {
		case err != nil:
			rep.Failed++
			rep.Failures[resealKind(err)]++
		case moved == resealWrote:
			rep.Moved++
			rep.From[from]++
		case moved == resealDry:
			rep.WouldMove++
			rep.From[from]++
		default:
			rep.Skipped++
		}
	}
	if rep.Failed > 0 {
		return rep, fmt.Errorf("gatewaykeys: %d credential(s) did not move; the rest did", rep.Failed)
	}
	return rep, nil
}

type resealResult int

const (
	resealNone resealResult = iota
	resealWrote
	resealDry
)

var (
	errUnreadable = errors.New("unreadable envelope")
	errSeal       = errors.New("seal the credential")
)

// resealKind names why a row failed, from a fixed set (the same kinds the gateway counts).
func resealKind(err error) string {
	switch {
	case errors.Is(err, errUnreadable), errors.Is(err, secretstore.ErrUnsupportedFormat):
		return ReasonFormat
	case errors.Is(err, secretstore.ErrUnknownRecipientKey):
		return ReasonUnknownKey
	case errors.Is(err, secretstore.ErrOpen), errors.Is(err, errSeal):
		return ReasonOpen
	}
	return "database"
}

func (r Resealer) one(ctx context.Context, org, id string, pub secretstore.RecipientPublicKey) (string, resealResult, error) {
	var old []byte
	err := r.DB.QueryRow(ctx, `SELECT credential FROM mcp_servers WHERE org_id = $1 AND id = $2`, org, id).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && old == nil) {
		return "", resealNone, nil
	}
	if err != nil {
		return "", resealNone, err
	}
	var env secretstore.EnvelopeV2
	if err := json.Unmarshal(old, &env); err != nil || env.Format != secretstore.FormatV2 {
		return "", resealNone, errUnreadable
	}
	if env.RecipientKeyID == pub.ID() {
		return "", resealNone, nil // moved by an earlier run or sealed to the active key by the API
	}
	b := mcpcredfile.Binding(org, id)
	plain, err := r.Keyring.Open(b, env)
	if err != nil {
		return "", resealNone, err
	}
	defer clear(plain)
	if r.DryRun {
		return env.RecipientKeyID, resealDry, nil
	}
	ne, err := secretstore.SealV2(pub, b, plain)
	if err != nil {
		return "", resealNone, fmt.Errorf("%w: %w", errSeal, err)
	}
	nb, err := json.Marshal(ne)
	if err != nil {
		return "", resealNone, err
	}
	tag, err := r.DB.Exec(ctx, `UPDATE mcp_servers SET credential = $3 WHERE org_id = $1 AND id = $2 AND credential = $4::jsonb`, org, id, nb, bytes.TrimSpace(old))
	if err != nil {
		return "", resealNone, err
	}
	if tag.RowsAffected() != 1 {
		return "", resealNone, nil
	}
	return env.RecipientKeyID, resealWrote, nil
}
