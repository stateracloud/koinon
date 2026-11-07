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
//	forget      give up on a lost KEK: mark what it sealed lost, start new home repositories
//	export      print the key set as a Secret manifest: the KEK backup and its restore
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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/kekhealth"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/tracedest"
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

// Columns lists every place Koinon stores an envelope, named by the constants the writers bind.
// Add new ones here, or status, rewrap, retire, reencrypt and the KEK Secret check would miss
// them; a test fails for any secretstore.Binding in the code whose table and column are not here.
var Columns = []Column{
	{Table: catalog.ProvidersTable, Column: catalog.KeyColumn},
	// Provider cost API keys, for the daily cost reconciliation (ADR 0056, amendment 2026-11-06).
	{Table: catalog.ProvidersTable, Column: catalog.CostKeyColumn},
	{Table: connections.Table, Column: connections.Column},
	// Uncommitted work saved from a stuck sandbox (ADR 0054); NULL once pushed or expired.
	{Table: savedwork.Table, Column: savedwork.Column},
	// Remote MCP server credentials (ADR 0057).
	{Table: mcpservers.ServersTable, Column: mcpservers.CredentialColumn},
	// Agent home repository passwords (ADR 0032); keyed by agent_id, see rowKey.
	{Table: checkpoint.Table, Column: checkpoint.Column},
	// The passwords of later repository epochs (ADR 0085 decision 13); keyed by binding_row.
	{Table: checkpoint.EpochsTable, Column: checkpoint.Column},
	// Run-trace destination credentials (ADR 0074): the org's, and the platform's single row.
	{Table: tracedest.DestinationsTable, Column: tracedest.CredentialsColumn},
	{Table: tracedest.PlatformTable, Column: tracedest.CredentialsColumn},
}

// singleton is the platform's one-row table: it has no org_id or id, and its binding names the org
// and row "platform".
func (c Column) singleton() bool { return c.Table == tracedest.PlatformTable }

// rowKey is the column that names a row in the binding's RowID: id, except where the table is
// keyed by the agent (one row per agent) or by the agent and its repository epoch (binding_row,
// checkpoint.EpochRow).
func (c Column) rowKey() string {
	switch c.Table {
	case checkpoint.Table:
		return "agent_id"
	case checkpoint.EpochsTable:
		return "binding_row"
	}
	return "id"
}

// KeySet is the KEK Secret's content.
type KeySet struct {
	// Keys are the unwrapped KEKs.
	Keys map[string][]byte
	// Stored is each loaded KEK as the Secret holds it (secretstore.StoredKEK). Save keeps it for
	// the KEKs it has and wraps the others (new ones) with the store's primary wrapper.
	Stored map[string]secretstore.StoredKEK
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
	// Prompt asks the person for what is missing and confirms what is hard to undo. Nil means do
	// not ask: the commands then behave as they do in a script.
	Prompt *cli.Prompter
	// Yes skips the confirmations (--yes). It does not pick a KEK for the person.
	Yes bool
	// SecretName names the Secret in Export's manifest (default ExportName).
	SecretName string
	// Wrappers are the key-wrapping keys, for the reachability line of status; the zero value is the
	// local KEK alone.
	Wrappers secretstore.Wrappers
	// UsedID tells whether the database carries an envelope under a KEK ID. Add asks it; nil means
	// the probe on Pool.
	UsedID func(ctx context.Context, id string) (bool, error)
}

// confirm asks unless there is nobody to ask or --yes was given.
func (t *Tool) confirm(ctx context.Context, title, description string) error {
	if t.Prompt == nil || t.Yes {
		return nil
	}
	ok, err := t.Prompt.Confirm(ctx, title, description)
	if err != nil {
		return err
	}
	if !ok {
		return cli.ErrCancelled
	}
	return nil
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
		rows, err := t.Pool.Query(ctx, fmt.Sprintf(
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

// UsedID reports whether any stored value carries an envelope under the KEK ID: the usage query
// that keys status and keys forget use, asked about one ID.
func UsedID(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	usage, err := (&Tool{Pool: pool}).Usage(ctx)
	if err != nil {
		return false, err
	}
	return usage[id] > 0, nil
}

// RefuseUsedID is the guard behind "IDs are never reused": it returns an error when the database
// still carries an envelope under id (a KEK that was removed from the Secret, or lost with its
// wrapping key), and when the probe cannot tell. A nil probe fails closed too.
func RefuseUsedID(ctx context.Context, used func(context.Context, string) (bool, error), id string) error {
	if used == nil {
		return fmt.Errorf("check that KEK ID %s is unused: no database to ask; IDs are never reused", id)
	}
	u, err := used(ctx, id)
	if err != nil {
		return fmt.Errorf("check that KEK ID %s is unused: %w", id, err)
	}
	if u {
		return fmt.Errorf("KEK %s seals values in the database although the Secret does not hold it; IDs are never reused: pick a new ID", id)
	}
	return nil
}

// AnyEnvelope reports whether any stored value is encrypted (so some KEK is needed to read it).
func AnyEnvelope(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	for _, c := range Columns {
		var found bool
		if err := pool.QueryRow(ctx, fmt.Sprintf(
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
		// Say what can be said without the keys: whether the wrapping key answers, and which KEKs
		// the bound-only policy refused (all of them, not only the first error).
		if errors.Is(err, secretstore.ErrUnreachable) && ks.Stored != nil {
			t.reachability(ctx, ks)
		}
		if errors.Is(err, secretstore.ErrBoundOnly) && ks.Stored != nil {
			t.boundOnlyRefused(ctx, ks)
		}
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
	_, _ = fmt.Fprintln(w, "KEK\tSTATE\tVALUES\tWRAPPED BY\tFINGERPRINT")
	for _, id := range ids {
		state, wrapped, fp := "retained", "-", "-"
		switch {
		case id == ks.Active:
			state = "active"
		case ks.Keys[id] == nil:
			state = "MISSING"
		}
		if k := ks.Keys[id]; k != nil {
			wrapped, fp = storedName(ks, id)+bindingNote(ks, id), secretstore.Fingerprint(k)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", id, state, usage[id], wrapped, fp)
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
	t.reachability(ctx, ks)
	if err := t.installBinding(ctx, ks); err != nil {
		return err
	}
	return t.fingerprintCheck(ctx, ks)
}

// bindingNote says whether a KMS-wrapped KEK is bound to the install (koinon-private#615): ", bound" or
// ", unbound" (wrapped before koinon-private#615). A local KEK cannot be bound and gets nothing.
func bindingNote(ks KeySet, id string) string {
	s, ok := ks.Stored[id]
	switch {
	case !ok || s.Backend == secretstore.BackendLocal:
		return ""
	case s.Bound():
		return ", bound"
	default:
		return ", unbound"
	}
}

// boundOnlyRefused prints the bound-only marker and every KEK of ks it makes this install refuse:
// the KMS KEKs stored unbound (koinon-private#615).
func (t *Tool) boundOnlyRefused(ctx context.Context, ks KeySet) {
	if t.Pool != nil {
		if b, err := installid.ReadBoundOnly(ctx, t.Pool); err == nil && b.Set {
			_, _ = fmt.Fprintf(t.Out, "Bound only: %s.\n", b)
		}
	}
	for _, id := range secretstore.UnboundKEKs(ks.Stored) {
		_, _ = fmt.Fprintf(t.Out, "REFUSED: KEK %s is stored unbound (wrapped before koinon-private#615) and this install accepts bound KEKs only. "+
			"Restore a KEK Secret taken after the KEKs were bound, or, while the database holds no encrypted values, delete the KEK Secret and rerun the key-admin Job.\n", id)
	}
}

// installBinding prints the install ID the KMS-wrapped KEKs are bound to, and names the KEKs that
// are not bound yet. A key set of local KEKs only says nothing: they cannot be bound.
func (t *Tool) installBinding(ctx context.Context, ks KeySet) error {
	var unbound []string
	kms := false
	for _, id := range slices.Sorted(maps.Keys(ks.Stored)) {
		s := ks.Stored[id]
		if s.Backend == secretstore.BackendLocal {
			continue
		}
		kms = true
		if !s.Bound() {
			unbound = append(unbound, id)
		}
	}
	if t.Pool != nil {
		b, err := installid.ReadBoundOnly(ctx, t.Pool)
		if err != nil {
			return err
		}
		if b.Set {
			_, _ = fmt.Fprintf(t.Out, "Bound only: %s. KEKs stored unbound (wrapped before koinon-private#615) never open here.\n", b)
		} else {
			_, _ = fmt.Fprintln(t.Out, "Bound only: not set. KEKs stored unbound (wrapped before koinon-private#615) still open; the key-admin Job sets it once every KEK is bound (secrets.kek.kms.keyAdmin.rewrapUnbound).")
		}
	}
	if !kms {
		return nil
	}
	if t.Pool != nil {
		install, err := installid.Read(ctx, t.Pool)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(t.Out, "The install ID is %s: KEKs wrapped by a KMS since koinon-private#615 are bound to it and open only next to this database.\n", install)
	}
	for _, id := range unbound {
		_, _ = fmt.Fprintf(t.Out, "KEK %s is not bound to the install (wrapped before koinon-private#615): it would also open next to another install's database. Bind it with the key-admin Job (secrets.kek.kms.keyAdmin.rewrapUnbound), or rotate it out.\n", id)
	}
	return nil
}

// reachability prints whether the active KEK's wrapping key answers a probe, and warns about a
// local active KEK on an install that opted in to a KMS (never on a local-only one).
func (t *Tool) reachability(ctx context.Context, ks KeySet) {
	active, ok := ks.Stored[ks.Active]
	if !ok {
		active = secretstore.StoredKEK{Backend: secretstore.BackendLocal}
	}
	if err := t.Wrappers.Probe(ctx, active); err != nil {
		_, _ = fmt.Fprintf(t.Out, "WARNING: the key-wrapping key of the active KEK %s (%s) is unreachable: %v. Values wrapped by it cannot be opened until it answers.\n", ks.Active, active, err)
	} else {
		_, _ = fmt.Fprintf(t.Out, "The key-wrapping key of the active KEK %s (%s) is reachable.\n", ks.Active, active)
	}
	if active.Backend == secretstore.BackendLocal && t.Wrappers.Primary().Backend() != secretstore.BackendLocal {
		_, _ = fmt.Fprintf(t.Out, "WARNING: the active KEK %s is a local key but a KMS (%s) is configured. Ask the key-admin Job to add a KEK, then activate it.\n", ks.Active, t.Wrappers.Primary().Backend())
	}
}

// fingerprintCheck compares the loaded KEKs with the fingerprints the database recorded for their
// IDs, and warns about a difference: a restored or wrong KEK Secret. Without a database it says so.
func (t *Tool) fingerprintCheck(ctx context.Context, ks KeySet) error {
	if t.Pool == nil {
		return nil
	}
	rec := kekhealth.Recorder{Q: store.New(t.Pool)}
	recorded, err := rec.Recorded(ctx)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		r, ok := recorded[id]
		if !ok {
			continue
		}
		if fp := secretstore.Fingerprint(ks.Keys[id]); fp != r.Fingerprint {
			_, _ = fmt.Fprintf(t.Out, "WARNING: KEK %s has fingerprint %s, but the database recorded %s for it. This is not the key the database was written with: a restored or wrong KEK Secret opens nothing. Restore the right one from your backup.\n", id, fp, r.Fingerprint)
		}
	}
	return nil
}

// storedName names what wraps KEK id: "local" or backend:key.
func storedName(ks KeySet, id string) string {
	if s, ok := ks.Stored[id]; ok {
		return s.String()
	}
	return secretstore.BackendLocal
}

// ExportName is the Secret name Export writes when the Tool has none.
const ExportName = "koinon-kek"

// Export writes the key set as a Kubernetes Secret manifest: the backup of the KEKs, and what a
// restore applies (kubectl apply -n <namespace> -f). It writes each KEK as the Secret holds it, so
// a KMS-wrapped KEK stays wrapped and a local KEK is the key itself: keep the file with your other
// secrets, apart from the database backups. The manifest has no namespace, so it restores into a
// fresh one (ADR 0085 decision 15), and a comment line per KEK gives its fingerprint.
func (t *Tool) Export(ctx context.Context) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	defer func() {
		for _, k := range ks.Keys {
			clear(k)
		}
	}()
	data := map[string][]byte{secretstore.ActiveFile: []byte(ks.Active)}
	for id := range ks.Keys {
		s, ok := ks.Stored[id]
		if !ok {
			return fmt.Errorf("KEK %s has no stored form to export", id)
		}
		if data[id], err = s.Encode(); err != nil {
			return fmt.Errorf("KEK %s: %w", id, err)
		}
	}
	name := t.SecretName
	if name == "" {
		name = ExportName
	}
	sec := corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{project.Domain + "/managed-by": "koinon-api"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if !ks.ActivatedAt.IsZero() {
		sec.Annotations = map[string]string{ActivatedAnnotation: ks.ActivatedAt.UTC().Format(time.RFC3339)}
	}
	out, err := yaml.Marshal(sec)
	if err != nil {
		return err
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "# Koinon KEK export, %s. Restore: kubectl apply -n <namespace> -f <this file>\n", t.now().UTC().Format(time.RFC3339))
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		_, _ = fmt.Fprintf(&b, "# %s fingerprint %s, wrapped by %s\n", id, secretstore.Fingerprint(ks.Keys[id]), storedName(ks, id))
	}
	_, _ = b.Write(out)
	_, err = io.WriteString(t.Out, b.String())
	return err
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
	// A KEK removed from the Secret may still seal values: a new KEK under its ID would be tried
	// against them, and keys forget would then refuse the ID.
	used := t.UsedID
	if used == nil && t.Pool != nil {
		pool := t.Pool
		used = func(ctx context.Context, id string) (bool, error) { return UsedID(ctx, pool, id) }
	}
	if err := RefuseUsedID(ctx, used, id); err != nil {
		return err
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
//
// With an empty id and a Prompt, the person picks one of the inactive KEKs.
func (t *Tool) Activate(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = t.pickInactive(ctx, ks); err != nil {
			return err
		}
	}
	if _, ok := ks.Keys[id]; !ok {
		return fmt.Errorf("KEK %s is not in the key set", id)
	}
	// An install that opted in to a KMS never activates a local KEK (procoduck, 2026-11-03). To roll back,
	// set the configured primary to local first (docs/ops/secret-keys.md), then activate.
	if p := t.Wrappers.Primary(); p.Backend() != secretstore.BackendLocal && ks.Stored[id].Backend == secretstore.BackendLocal {
		return fmt.Errorf("KEK %s is a local key and the KMS %s is configured; activate a KEK wrapped by the KMS (ask the key-admin Job to add one), or switch the configured wrapping to local first to roll back", id, p.Backend())
	}
	if err := t.confirm(ctx, fmt.Sprintf("Activate %s?", id), "New values use it within about a minute."); err != nil {
		return err
	}
	ks.Active, ks.ActivatedAt = id, t.now()
	if err := t.Store.Save(ctx, ks); err != nil {
		return err
	}
	t.recordActivation(ctx, ks)
	_, _ = fmt.Fprintf(t.Out, "KEK %s is active. Wait for the pods to pick it up (about a minute), then run: koinon-api keys rewrap\n", id)
	return nil
}

// recordActivation records the fingerprints of the key set and the activation time in the database,
// when there is one. The activation itself has happened, so a failure is printed, not returned.
func (t *Tool) recordActivation(ctx context.Context, ks KeySet) {
	if t.Pool == nil {
		return
	}
	l := secretstore.NewLoaded(ks.Stored, ks.Active, ks.Keys, nil)
	l.ActivatedAt = ks.ActivatedAt
	// A KEK added in this process has no stored form yet; Save wrapped it, so describe it as the store's primary does.
	stored := make(map[string]secretstore.StoredKEK, len(ks.Keys))
	for kid := range ks.Keys {
		if s, ok := ks.Stored[kid]; ok {
			stored[kid] = s
		} else {
			p := t.Wrappers.Primary()
			stored[kid] = secretstore.StoredKEK{Backend: p.Backend(), KeyID: p.KeyID()}
		}
	}
	l.Stored = stored
	mismatches, err := kekhealth.Recorder{Q: store.New(t.Pool)}.Record(ctx, l)
	if err != nil {
		_, _ = fmt.Fprintf(t.Out, "WARNING: the KEK fingerprints were not recorded in the database: %v\n", err)
	}
	for _, m := range mismatches {
		_, _ = fmt.Fprintf(t.Out, "WARNING: KEK %s has fingerprint %s, but the database recorded %s for it.\n", m.KEKID, m.Loaded, m.Recorded)
	}
}

// pickInactive lists the KEKs that are not active, with their age and (when there is a database)
// the values each wraps, and returns the one the person picks.
func (t *Tool) pickInactive(ctx context.Context, ks KeySet) (string, error) {
	if t.Prompt == nil {
		return "", errors.New("a KEK ID is required")
	}
	var usage map[string]int
	if t.Pool != nil {
		var err error
		if usage, err = t.Usage(ctx); err != nil {
			return "", err
		}
	}
	var choices []cli.Choice
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		if id != ks.Active {
			choices = append(choices, cli.Choice{Label: t.describeKEK(id, usage), Value: id})
		}
	}
	if len(choices) == 0 {
		return "", errors.New("there is no inactive KEK to activate; create one with: koinon-api keys add")
	}
	return t.Prompt.Select(ctx, "Which KEK should wrap new values?", choices)
}

// describeKEK is the picker's line for id: the ID, its age when the ID carries a date, and the
// values it wraps when usage is known.
func (t *Tool) describeKEK(id string, usage map[string]int) string {
	label := id
	if added, ok := ageFromID(id); ok {
		label += fmt.Sprintf("  added %d days ago", int(t.now().Sub(added).Hours()/24))
	}
	if usage != nil {
		label += fmt.Sprintf("  wraps %d value(s)", usage[id])
	}
	return label
}

func (t *Tool) ring(ks KeySet) (*secretstore.Keyring, error) {
	return secretstore.NewKeyring(ks.Keys, ks.Active)
}

// Rewrap wraps every stored data key under the active KEK. It never decrypts a payload.
func (t *Tool) Rewrap(ctx context.Context) error {
	if t.Prompt != nil && !t.Yes {
		ks, usage, err := t.keysAndUsage(ctx)
		if err != nil {
			return err
		}
		n := 0
		for id, c := range usage {
			if id != ks.Active {
				n += c
			}
		}
		if err := t.confirm(ctx, fmt.Sprintf("Rewrap %d value(s) under %s?", n, ks.Active),
			"Data keys are wrapped again; payloads are not decrypted."); err != nil {
			return err
		}
	}
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
	if t.Prompt != nil && !t.Yes {
		ks, usage, err := t.keysAndUsage(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, c := range usage {
			n += c
		}
		got, err := t.Prompt.Input(ctx,
			fmt.Sprintf("Re-encrypt %d value(s) under new data keys? Type the active KEK ID (%s) to continue", n, ks.Active),
			"This decrypts every payload in memory. It does not undo a leak: rotate the credentials at their providers afterwards.",
			func(s string) error {
				if strings.TrimSpace(s) != ks.Active {
					return fmt.Errorf("type %s to continue", ks.Active)
				}
				return nil
			})
		if err != nil {
			return err
		}
		if strings.TrimSpace(got) != ks.Active {
			return cli.ErrCancelled
		}
	}
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

func (t *Tool) keysAndUsage(ctx context.Context) (KeySet, map[string]int, error) {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return KeySet{}, nil, err
	}
	usage, err := t.Usage(ctx)
	return ks, usage, err
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
	if c.singleton() {
		var found bool
		if err := t.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s IS NOT NULL)`, c.Table, c.Column)).Scan(&found); err != nil || !found {
			return nil, err
		}
		return []rowRef{{org: "platform", id: "platform"}}, nil
	}
	rows, err := t.Pool.Query(ctx, fmt.Sprintf(
		`SELECT org_id::text, %[3]s::text FROM %[1]s WHERE %[2]s IS NOT NULL ORDER BY org_id, %[3]s`, c.Table, c.Column, c.rowKey()))
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
		sel, upd := `WHERE org_id = $1 AND %[3]s = $2`, `WHERE org_id = $1 AND %[3]s = $2`
		if c.singleton() {
			sel, upd = `WHERE singleton AND $1 = $2`, `WHERE singleton AND $1 = $2`
		}
		err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT %[2]s FROM %[1]s `+sel+` FOR UPDATE`, c.Table, c.Column, c.rowKey()), r.org, r.id).Scan(&raw)
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
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %[1]s SET %[2]s = $3 `+upd, c.Table, c.Column, c.rowKey()), r.org, r.id, nb); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// Retire removes a KEK from the key set. It refuses while the KEK is active or any value
// uses it. With an empty id and a Prompt, the person picks one of the KEKs that can be retired.
func (t *Tool) Retire(ctx context.Context, id string) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	usage, err := t.Usage(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = t.pickRetirable(ctx, ks, usage); err != nil {
			return err
		}
	}
	if err := t.retireBlock(ks, usage, id); err != nil {
		return err
	}
	if err := t.confirm(ctx, fmt.Sprintf("Retire %s?", id),
		"The KEK is removed from the key set. Keep a copy in your backup escrow for as long as database backups that used it exist."); err != nil {
		return err
	}
	delete(ks.Keys, id)
	if err := t.Store.Save(ctx, ks); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(t.Out, "Retired KEK %s. Keep a copy in your backup escrow for as long as database backups that used it exist.\n", id)
	return nil
}

// retireBlock is why id cannot be retired now, or nil.
func (t *Tool) retireBlock(ks KeySet, usage map[string]int, id string) error {
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
	if n := usage[id]; n > 0 {
		return fmt.Errorf("%d value(s) still use KEK %s; run \"keys rewrap\" first (or \"keys reencrypt\")", n, id)
	}
	return nil
}

// pickRetirable lists every KEK; the ones retireBlock refuses are shown with the reason.
func (t *Tool) pickRetirable(ctx context.Context, ks KeySet, usage map[string]int) (string, error) {
	if t.Prompt == nil {
		return "", errors.New("a KEK ID is required")
	}
	var choices []cli.Choice
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		c := cli.Choice{Label: t.describeKEK(id, usage), Value: id}
		if err := t.retireBlock(ks, usage, id); err != nil {
			c.Disabled = err.Error()
		}
		choices = append(choices, c)
	}
	if allDisabled(choices) {
		for _, c := range choices {
			_, _ = fmt.Fprintf(t.Out, "  %s (not available: %s)\n", c.Label, c.Disabled)
		}
		return "", errors.New("no KEK can be retired now (reasons above)")
	}
	return t.Prompt.Select(ctx, "Which KEK should be retired?", choices)
}

func allDisabled(cs []cli.Choice) bool {
	for _, c := range cs {
		if c.Disabled == "" {
			return false
		}
	}
	return true
}
