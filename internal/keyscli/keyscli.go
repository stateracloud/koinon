// SPDX-License-Identifier: Apache-2.0

// Package keyscli implements "koinon-api keys": managing the key-encryption keys (KEKs) that
// wrap the data keys of Koinon's stored secrets (ADR 0056, plan item PV1; the design and its
// sources are in docs/research/kek-rotation.md, the procedures in docs/ops/secret-keys.md).
//
//	status      list the KEKs, which one is active and how many values each wraps
//	add         generate a new KEK (not active yet)
//	activate    make a KEK the one that wraps new data keys
//	rewrap      wrap every stored data key under the active KEK; payloads are not decrypted
//	retire      remove a KEK, only when no value uses it
//	reencrypt   seal every value again under a new data key: the compromise path
package keyscli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// DefaultActivePeriod is how long a KEK stays active before status warns (a year, like AWS KMS's
// default rotation; NIST SP 800-57 allows up to two for a key-wrapping key).
const DefaultActivePeriod = 365 * 24 * time.Hour

// MinActiveBeforeRetire is how long the active KEK must have been active before another one can
// be retired.
const MinActiveBeforeRetire = 5 * time.Minute

// Column is a column that holds envelopes. The table and column names are part of every
// envelope's AAD, so they must match what the writers bind.
type Column struct {
	Table  string
	Column string
}

// Columns lists every place Koinon stores an envelope. Add new ones here, or status, rewrap,
// retire and reencrypt would miss them.
var Columns = []Column{
	{Table: "providers", Column: "api_key"},
	{Table: "connections", Column: "secret_envelope"},
}

// KeySet is the KEK Secret's content.
type KeySet struct {
	Keys   map[string][]byte
	Active string
	// ActivatedAt is when Active became active, if known.
	ActivatedAt time.Time
}

// Store loads and saves the KeySet.
type Store interface {
	Load(ctx context.Context) (KeySet, error)
	Save(ctx context.Context, ks KeySet) error
}

// Tool runs the commands.
type Tool struct {
	Pool         *pgxpool.Pool
	Store        Store
	Out          io.Writer
	ActivePeriod time.Duration
	Now          func() time.Time
}

func (t *Tool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// NewID returns a KEK ID that carries its creation date: kek-YYYYMMDD-xxxx.
func NewID(now time.Time) (string, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "kek-" + now.UTC().Format("20060102") + "-" + hex.EncodeToString(b), nil
}

// Usage counts the stored values per KEK ID.
func (t *Tool) Usage(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, c := range Columns {
		rows, err := t.Pool.Query(ctx, fmt.Sprintf( //nolint:gosec // names come from Columns
			`SELECT %[2]s->>'kek_id', count(*) FROM %[1]s WHERE %[2]s IS NOT NULL GROUP BY 1`, c.Table, c.Column))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] += n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AnyEnvelope reports whether any stored value is encrypted (so some KEK is needed to read it).
func AnyEnvelope(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	for _, c := range Columns {
		var found bool
		if err := pool.QueryRow(ctx, fmt.Sprintf( //nolint:gosec // names come from Columns
			`SELECT EXISTS (SELECT 1 FROM %s WHERE %s IS NOT NULL)`, c.Table, c.Column)).Scan(&found); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func ageFromID(id string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(id, "kek-")
	if !ok || len(rest) < 8 {
		return time.Time{}, false
	}
	d, err := time.Parse("20060102", rest[:8])
	return d, err == nil
}

// Status prints the KEKs and warns about the active one's age and about values whose KEK is
// missing. It returns an error only when it cannot read.
func (t *Tool) Status(ctx context.Context) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	usage, err := t.Usage(ctx)
	if err != nil {
		return err
	}
	ids := slices.Sorted(maps.Keys(ks.Keys))
	for id := range usage {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	w := tabwriter.NewWriter(t.Out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KEK\tSTATE\tVALUES")
	for _, id := range ids {
		state := "retained"
		switch {
		case id == ks.Active:
			state = "active"
		case ks.Keys[id] == nil:
			state = "MISSING"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\n", id, state, usage[id])
	}
	_ = w.Flush()
	period := t.ActivePeriod
	if period <= 0 {
		period = DefaultActivePeriod
	}
	since := ks.ActivatedAt
	if since.IsZero() {
		since, _ = ageFromID(ks.Active)
	}
	switch {
	case since.IsZero():
		_, _ = fmt.Fprintf(t.Out, "\nThe active KEK %s has no recorded activation date; rotate it every %s.\n", ks.Active, period)
	case t.now().Sub(since) > period:
		_, _ = fmt.Fprintf(t.Out, "\nWARNING: the active KEK %s has been active for %d days, past the %d-day period. Rotate it: keys add, keys activate, keys rewrap, keys retire.\n",
			ks.Active, int(t.now().Sub(since).Hours()/24), int(period.Hours()/24))
	default:
		_, _ = fmt.Fprintf(t.Out, "\nThe active KEK %s has been active for %d of %d days.\n", ks.Active, int(t.now().Sub(since).Hours()/24), int(period.Hours()/24))
	}
	for _, id := range ids {
		if ks.Keys[id] == nil {
			_, _ = fmt.Fprintf(t.Out, "WARNING: %d value(s) are wrapped by KEK %s, which is not in the key set. They cannot be opened until the key is restored from your backup.\n", usage[id], id)
		}
	}
	return nil
}

// Add generates a KEK, stores it in the key set (inactive) and prints its ID.
func (t *Tool) Add(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = NewID(t.now()); err != nil {
			return err
		}
	}
	if !secretstore.ValidKEKID(id) {
		return fmt.Errorf("%q is not a valid KEK ID", id)
	}
	if _, ok := ks.Keys[id]; ok {
		return fmt.Errorf("KEK %s already exists; IDs are never reused", id)
	}
	key := make([]byte, secretstore.KeySize)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	ks.Keys[id] = key
	if err := t.Store.Save(ctx, ks); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(t.Out, "Added KEK %s. Back up the KEK Secret now, then run: koinon-api keys activate %s\n", id, id)
	return nil
}

// Activate makes id the wrapping KEK. New values use it once the pods have the changed Secret
// (kubelet propagates it within about a minute).
func (t *Tool) Activate(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	if _, ok := ks.Keys[id]; !ok {
		return fmt.Errorf("KEK %s is not in the key set", id)
	}
	ks.Active, ks.ActivatedAt = id, t.now()
	if err := t.Store.Save(ctx, ks); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(t.Out, "KEK %s is active. Wait for the pods to pick it up (about a minute), then run: koinon-api keys rewrap\n", id)
	return nil
}

func (t *Tool) ring(ks KeySet) (*secretstore.Keyring, error) {
	return secretstore.NewKeyring(ks.Keys, ks.Active)
}

// Rewrap wraps every stored data key under the active KEK. It never decrypts a payload.
func (t *Tool) Rewrap(ctx context.Context) error {
	return t.each(ctx, "rewrapped", func(ring *secretstore.Keyring, b secretstore.Binding, env secretstore.Envelope, active string) (*secretstore.Envelope, error) {
		if env.KEKID == active {
			return nil, nil
		}
		out, err := ring.Rewrap(b, env)
		return &out, err
	})
}

// Reencrypt seals every value again under a fresh data key, for a suspected compromise. It
// decrypts each payload in memory. After it, rotate the credentials themselves at their
// providers: re-encrypting does not undo a leak.
func (t *Tool) Reencrypt(ctx context.Context) error {
	return t.each(ctx, "re-encrypted", func(ring *secretstore.Keyring, b secretstore.Binding, env secretstore.Envelope, _ string) (*secretstore.Envelope, error) {
		plain, err := ring.Open(b, env)
		if err != nil {
			return nil, err
		}
		defer clear(plain)
		out, err := ring.Seal(b, plain)
		return &out, err
	})
}

type rowFn func(ring *secretstore.Keyring, b secretstore.Binding, env secretstore.Envelope, active string) (*secretstore.Envelope, error)

func (t *Tool) each(ctx context.Context, verb string, fn rowFn) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	ring, err := t.ring(ks)
	if err != nil {
		return err
	}
	total := 0
	var errs []error
	for _, c := range Columns {
		ids, err := t.rowIDs(ctx, c)
		if err != nil {
			return err
		}
		for _, r := range ids {
			changed, err := t.one(ctx, c, r, ring, ks.Active, fn)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", c.Table, r.id, err))
				continue
			}
			if changed {
				total++
			}
		}
	}
	_, _ = fmt.Fprintf(t.Out, "%d value(s) %s.\n", total, verb)
	return errors.Join(errs...)
}

type rowRef struct{ org, id string }

func (t *Tool) rowIDs(ctx context.Context, c Column) ([]rowRef, error) {
	rows, err := t.Pool.Query(ctx, fmt.Sprintf( //nolint:gosec // names come from Columns
		`SELECT org_id::text, id::text FROM %s WHERE %s IS NOT NULL ORDER BY org_id, id`, c.Table, c.Column))
	if err != nil {
		return nil, err
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

func (t *Tool) one(ctx context.Context, c Column, r rowRef, ring *secretstore.Keyring, active string, fn rowFn) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, t.Pool, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, fmt.Sprintf( //nolint:gosec // names come from Columns
			`SELECT %[2]s FROM %[1]s WHERE org_id = $1 AND id = $2 FOR UPDATE`, c.Table, c.Column), r.org, r.id).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) || raw == nil {
			return nil // deleted meanwhile
		}
		if err != nil {
			return err
		}
		var env secretstore.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return errors.New("unreadable envelope")
		}
		out, err := fn(ring, secretstore.Binding{OrgID: r.org, Table: c.Table, RowID: r.id, Column: c.Column}, env, active)
		if err != nil {
			return err
		}
		if out == nil {
			return nil
		}
		nb, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf( //nolint:gosec // names come from Columns
			`UPDATE %[1]s SET %[2]s = $3 WHERE org_id = $1 AND id = $2`, c.Table, c.Column), r.org, r.id, nb); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// Retire removes a KEK from the key set. It refuses while the KEK is active or any value
// uses it.
func (t *Tool) Retire(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	if _, ok := ks.Keys[id]; !ok {
		return fmt.Errorf("KEK %s is not in the key set", id)
	}
	if id == ks.Active {
		return fmt.Errorf("KEK %s is the active KEK; activate another one first", id)
	}
	// Pods pick up an activation within about a minute (kubelet propagation, then the reload tick).
	// Until then they may still wrap with the previous KEK, and retiring it would strand those values.
	if !ks.ActivatedAt.IsZero() && t.now().Sub(ks.ActivatedAt) < MinActiveBeforeRetire {
		return fmt.Errorf("the active KEK %s was activated %s ago; wait until it has been active for %s so every pod wraps with it, rewrap, then retire",
			ks.Active, t.now().Sub(ks.ActivatedAt).Round(time.Second), MinActiveBeforeRetire)
	}
	usage, err := t.Usage(ctx)
	if err != nil {
		return err
	}
	if n := usage[id]; n > 0 {
		return fmt.Errorf("%d value(s) still use KEK %s; run \"keys rewrap\" first (or \"keys reencrypt\")", n, id)
	}
	delete(ks.Keys, id)
	if err := t.Store.Save(ctx, ks); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(t.Out, "Retired KEK %s. Keep a copy in your backup escrow for as long as database backups that used it exist.\n", id)
	return nil
}
