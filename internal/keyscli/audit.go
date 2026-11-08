// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// The key events of the install audit chain (ADR 0100 decision 3, "Keys: not atomic, so checked on
// start"). A Secret update is not a database write, so each event is written right after the change
// it records, and koinon-api compares the whole key-set state on every start (Auditor.Observe) to
// catch an event that was lost.
const (
	ActionAdd           = "keys.add"
	ActionActivate      = "keys.activate"
	ActionRetire        = "keys.retire"
	ActionRewrap        = "keys.rewrap"
	ActionReencrypt     = "keys.reencrypt"
	ActionGenerated     = "keys.generated"
	ActionSecretCreated = "keys.secret_created"
	ActionBound         = "keys.bound"
	ActionObserved      = "keys.observed"
	// StartedSuffix is appended to ActionRewrap or ActionReencrypt for the optional event written
	// before the per-row loop.
	StartedSuffix = ".started"
)

// Actors of the key events (ADR 0100 decision 2). The CLI's actor is audit.CLI("keys <command>").
var (
	// ActorKeys is koinon-api generating a key itself: the first KEK, the saved-work key.
	ActorKeys = audit.System("koinon-api:keys")
	// ActorKeyAdmin is the key-admin Job.
	ActorKeyAdmin = audit.System("koinon-api:key-admin")
	// ActorStartup is koinon-api's start-up observation.
	ActorStartup = audit.System("koinon-api:startup")
	// ActorKeyringWatch is a running koinon-api's check after its keyring watch saw the key set change.
	ActorKeyringWatch = audit.System("koinon-api:keyring-watch")
)

// GeneratedLabel marks a Secret that Koinon generated (the ensure-secrets Job's); a label-selected
// list of it finds them without reading any value.
const GeneratedLabel = project.Domain + "/generated"

// advisoryLockName serialises the start-up check across replicas.
const advisoryLockName = "koinon.keys_observed"

// KEKState is one KEK of the key set as an audit event shows it: its ID and fingerprint (as "keys
// status" prints them), never the key.
type KEKState struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	// WrappedBy is "local" or "backend:key", the wrapping key's name.
	WrappedBy string `json:"wrapped_by"`
	// Binding is "bound" or "unbound" for a KMS-wrapped KEK; empty for a local one.
	Binding string `json:"binding,omitempty"`
}

// GeneratedSecret is the metadata of a Secret Koinon generated: name, UID and creation time. A
// re-created Secret has a new UID, which is how a replaced key shows. Never a value.
type GeneratedSecret struct {
	SecretName string `json:"secret_name"`
	UID        string `json:"uid"`
	CreatedAt  string `json:"created_at"`
}

// KeyState is the whole key-set state a key event records in its summary's "after" (and "before"):
// what koinon-api compares on start.
type KeyState struct {
	KEKs []KEKState `json:"keks"`
	// Active is the active KEK's ID.
	Active string `json:"active"`
	// Retired are the IDs of KEKs that were in the key set and no longer are, oldest knowledge first
	// by ID. A KEK that comes back leaves the list.
	Retired   []string          `json:"retired"`
	Generated []GeneratedSecret `json:"generated"`
}

// normalized returns s with nil lists made empty and every list sorted, so equal states compare
// and print equal.
func (s KeyState) normalized() KeyState {
	s.KEKs = slices.Clone(s.KEKs)
	slices.SortFunc(s.KEKs, func(a, b KEKState) int { return cmpString(a.ID, b.ID) })
	if s.KEKs == nil {
		s.KEKs = []KEKState{}
	}
	s.Retired = slices.Clone(s.Retired)
	slices.Sort(s.Retired)
	s.Retired = slices.Compact(s.Retired)
	if s.Retired == nil {
		s.Retired = []string{}
	}
	s.Generated = slices.Clone(s.Generated)
	slices.SortFunc(s.Generated, func(a, b GeneratedSecret) int { return cmpString(a.SecretName, b.SecretName) })
	if s.Generated == nil {
		s.Generated = []GeneratedSecret{}
	}
	return s
}

// Normalized returns s with nil lists made empty and every list sorted, as a recorded state has them.
func (s KeyState) Normalized() KeyState { return s.normalized() }

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Equal reports whether two states describe the same key set.
func (s KeyState) Equal(o KeyState) bool {
	a, b := s.normalized(), o.normalized()
	return a.Active == b.Active && slices.Equal(a.KEKs, b.KEKs) && slices.Equal(a.Retired, b.Retired) && slices.Equal(a.Generated, b.Generated)
}

// stateOf describes ks. prev is the last recorded state (nil if none): it supplies the retired KEKs
// (those it had that ks no longer holds, added to those it already retired) and, unless
// authoritative, the generated Secrets that gen does not mention. gen is what the writer could read;
// with authoritative it is the whole list.
func stateOf(ks KeySet, prev *KeyState, gen []GeneratedSecret, authoritative bool) KeyState {
	var st KeyState
	st.Active = ks.Active
	for _, id := range slices.Sorted(maps.Keys(ks.Keys)) {
		k := KEKState{ID: id, Fingerprint: secretstore.Fingerprint(ks.Keys[id]), WrappedBy: secretstore.BackendLocal}
		if s, ok := ks.Stored[id]; ok {
			k.WrappedBy = s.String()
			if s.Backend != secretstore.BackendLocal {
				k.Binding = "unbound"
				if s.Bound() {
					k.Binding = "bound"
				}
			}
		}
		st.KEKs = append(st.KEKs, k)
	}
	retired := map[string]bool{}
	if prev != nil {
		for _, id := range prev.Retired {
			retired[id] = true
		}
		for _, k := range prev.KEKs {
			retired[k.ID] = true
		}
	}
	for _, k := range st.KEKs {
		delete(retired, k.ID)
	}
	st.Retired = slices.Sorted(maps.Keys(retired))
	merged := map[string]GeneratedSecret{}
	if prev != nil && !authoritative {
		for _, g := range prev.Generated {
			merged[g.SecretName] = g
		}
	}
	for _, g := range gen {
		merged[g.SecretName] = g
	}
	for _, name := range slices.Sorted(maps.Keys(merged)) {
		st.Generated = append(st.Generated, merged[name])
	}
	return st.normalized()
}

// GeneratedReader reads the metadata of the Secrets Koinon generated, and nothing else: name, UID and
// creation time. The values of a Secret travel in the same API response (Kubernetes cannot leave
// them out of a typed list), but no field of Data is ever read, kept or logged.
type GeneratedReader struct {
	Client    kubernetes.Interface
	Namespace string
	// ListLabelled also lists the Secrets labelled GeneratedLabel. It needs the list verb on Secrets,
	// which koinon-api has and the key-admin Job (get and update on the KEK Secret by name) does not.
	ListLabelled bool
	// Names are Secrets read by name, for the ones no label marks: the KEK Secret and the saved-work
	// key. A name that does not exist is skipped.
	Names []string
}

// Read lists the Secrets labelled GeneratedLabel (with ListLabelled) and gets those in Names. koinon-api
// already has the get and list verbs on Secrets it needs (charts/koinon/templates/rbac.yaml).
func (r GeneratedReader) Read(ctx context.Context) ([]GeneratedSecret, error) {
	secrets := r.Client.CoreV1().Secrets(r.Namespace)
	byName := map[string]GeneratedSecret{}
	add := func(s *corev1.Secret) {
		byName[s.Name] = GeneratedSecret{SecretName: s.Name, UID: string(s.UID), CreatedAt: s.CreationTimestamp.UTC().Format(time.RFC3339)}
	}
	if r.ListLabelled {
		list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: GeneratedLabel})
		if err != nil {
			return nil, fmt.Errorf("list the generated Secrets: %w", err)
		}
		for i := range list.Items {
			add(&list.Items[i])
		}
	}
	for _, name := range r.Names {
		if name == "" {
			continue
		}
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the Secret %s: %w", name, err)
		}
		add(s)
	}
	out := make([]GeneratedSecret, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[name])
	}
	return out, nil
}

// ErrNotAudited is in the error of a key change that was made but whose install audit event could
// not be written. The change stands; the next koinon-api start records the key-set state as
// keys.observed.
var ErrNotAudited = errors.New("the change was made, but its install audit event was not recorded; the next koinon-api start records the key set as keys.observed")

// Auditor writes the key events of one writer. The zero Actor is a CLI actor, named by command.
type Auditor struct {
	Pool *pgxpool.Pool
	// Actor names the writer when it is not the CLI (the key-admin Job, koinon-api itself).
	Actor audit.Actor
	// Generated reads the generated Secrets the writer can see; nil reads none.
	Generated func(ctx context.Context) ([]GeneratedSecret, error)
	// Authoritative says Generated lists every generated Secret: the state then holds exactly those.
	// Otherwise they are merged over the ones the last recorded state held, so a writer that cannot
	// see the saved-work key (the CLI, the Job) does not erase it from the state.
	Authoritative bool
	// Wait is how long the Auditor retries, in all its calls together, while the chain cannot take an
	// event or be read yet (the table is not migrated, the writer role is not granted): the key-admin
	// Job runs next to the migration Job. The clock starts at the first call, so a Job whose chain
	// stays unreadable waits Wait once, not once per call.
	Wait time.Duration
	// Write replaces the insert, for tests.
	Write func(ctx context.Context, e audit.InstallEvent) error

	mu       sync.Mutex
	deadline time.Time
}

// budget is the instant the retries of this Auditor end, the same for every call.
func (a *Auditor) budget() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deadline.IsZero() {
		a.deadline = time.Now().Add(a.Wait)
	}
	return a.deadline
}

func (a *Auditor) actor(command string) audit.Actor {
	if a.Actor.Kind != "" {
		return a.Actor
	}
	return audit.CLI("keys " + command)
}

func (a *Auditor) generated(ctx context.Context) ([]GeneratedSecret, error) {
	if a.Generated == nil {
		return nil, nil
	}
	return a.Generated(ctx)
}

// lastState is the "after" of the newest key event that has a key-set state, or nil.
func lastState(ctx context.Context, q *store.Queries) (*KeyState, error) {
	row, err := q.GetLastInstallKeyEvent(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the last recorded key state: %w", err)
	}
	var summary struct {
		After KeyState `json:"after"`
	}
	if err := json.Unmarshal(row.Summary, &summary); err != nil {
		return nil, fmt.Errorf("read the last recorded key state: %w", err)
	}
	st := summary.After.normalized()
	return &st, nil
}

// LastState returns the key-set state of the newest key event, or nil when none was recorded.
func (a *Auditor) LastState(ctx context.Context) (*KeyState, error) {
	return lastState(ctx, store.New(a.Pool))
}

// Snapshot describes ks against the last recorded state. Take it before a change, for the event's
// "before", and pass it to After.
func (a *Auditor) Snapshot(ctx context.Context, ks KeySet) (KeyState, error) {
	prev, err := retryChain(ctx, a.budget(), a.LastState)
	if err != nil {
		return KeyState{}, err
	}
	gen, err := a.generated(ctx)
	if err != nil {
		return KeyState{}, err
	}
	return stateOf(ks, prev, gen, a.Authoritative), nil
}

// After describes ks, as loaded again after a change, relative to before.
func (a *Auditor) After(ctx context.Context, ks KeySet, before KeyState) (KeyState, error) {
	gen, err := a.generated(ctx)
	if err != nil {
		return KeyState{}, err
	}
	return stateOf(ks, &before, gen, a.Authoritative), nil
}

// Event is one key event to write.
type Event struct {
	// Command is the keys subcommand for a CLI actor ("add", "activate", ...).
	Command    string
	Action     string
	TargetKind string
	TargetID   string
	Before     any
	After      any
}

// Record writes e, retrying for Wait while the chain cannot take it yet.
func (a *Auditor) Record(ctx context.Context, e Event) error {
	ev := audit.InstallEvent{Actor: a.actor(e.Command), Action: e.Action, TargetKind: e.TargetKind, TargetID: e.TargetID, Before: e.Before, After: e.After}
	_, err := retryChain(ctx, a.budget(), func(ctx context.Context) (struct{}, error) {
		if a.Write != nil {
			return struct{}{}, a.Write(ctx, ev)
		}
		_, err := audit.InTx(ctx, a.Pool, func(_ *store.Queries, b audit.Batch) error {
			b.Install(ev)
			return nil
		})
		return struct{}{}, err
	})
	if err != nil {
		return fmt.Errorf("write the %s install audit event: %w", e.Action, err)
	}
	return nil
}

// notReady reports an error that goes away once the migration and the grant have run.
func notReady(err error) bool {
	if errors.Is(err, audit.ErrInstallWriteDenied) {
		return true
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == "42P01" || pgErr.Code == "3F000" // undefined_table, invalid_schema_name
	}
	return false
}

// retryChain runs f, and again every two seconds while the chain is not ready and deadline has not passed.
func retryChain[T any](ctx context.Context, deadline time.Time, f func(context.Context) (T, error)) (T, error) {
	for {
		v, err := f(ctx)
		if err == nil || !notReady(err) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// ObserveResult says what Observe did.
type ObserveResult struct {
	// Wrote is true when it wrote keys.observed.
	Wrote bool
	// Before is the last recorded state (nil if none), After the state it found.
	Before *KeyState
	After  KeyState
}

// KeyLoader loads the live key set. The checks call it inside their transaction, after the advisory
// lock, so what they compare is never older than the last recorded event.
type KeyLoader func(ctx context.Context) (KeySet, error)

// sameKEKs reports whether two states hold the same KEKs, active KEK and retired KEKs.
func sameKEKs(a, b KeyState) bool {
	a, b = a.normalized(), b.normalized()
	return a.Active == b.Active && slices.Equal(a.KEKs, b.KEKs) && slices.Equal(a.Retired, b.Retired)
}

// lockKeys takes the advisory lock of the key events in tx. It is the first lock a key transaction
// takes; the install head, which the event insert locks, comes last (ADR 0100 decision 1.7).
func lockKeys(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryLockName)
	return err
}

// KeyLoadError is the error of a KeyLoader called under the lock: the key set could not be loaded, which
// is not a problem of the chain.
type KeyLoadError struct{ Err error }

func (e *KeyLoadError) Error() string { return "load the key set: " + e.Err.Error() }
func (e *KeyLoadError) Unwrap() error { return e.Err }

// compare loads the key set and the generated Secrets in tx, which holds the advisory lock, and compares
// them with the last recorded state. With full it compares the whole state, a first start included,
// and as the authoritative list of generated Secrets; otherwise only the KEK part, and never when
// nothing was recorded yet. It returns the keys.observed event to write, as actor, when they differ,
// and the loaded key set, which the caller clears.
func (a *Auditor) compare(ctx context.Context, tx pgx.Tx, load KeyLoader, actor audit.Actor, full bool) (ObserveResult, KeySet, *audit.InstallEvent, error) {
	ks, err := load(ctx)
	if err != nil {
		clearKeySet(ks)
		return ObserveResult{}, KeySet{}, nil, &KeyLoadError{Err: err}
	}
	gen, err := a.generated(ctx)
	if err != nil {
		clearKeySet(ks)
		return ObserveResult{}, KeySet{}, nil, err
	}
	prev, err := lastState(ctx, store.New(tx))
	if err != nil {
		clearKeySet(ks)
		return ObserveResult{}, KeySet{}, nil, err
	}
	cur := stateOf(ks, prev, gen, full || a.Authoritative)
	res := ObserveResult{Before: prev, After: cur}
	if full && (prev == nil || !prev.Equal(cur)) || !full && prev != nil && !sameKEKs(*prev, cur) {
		ev := &audit.InstallEvent{Actor: actor, Action: ActionObserved, TargetKind: "keys", TargetID: "keyset", After: cur}
		if prev != nil {
			ev.Before = *prev
		}
		return res, ks, ev, nil
	}
	return res, ks, nil, nil
}

func clearKeySet(ks KeySet) {
	for _, k := range ks.Keys {
		clear(k)
	}
}

// Observe is the start-up check (ADR 0100 decision 3): it compares the whole key-set state (KEK IDs
// and fingerprints, the active KEK, the retired KEKs, the generated Secrets' name, UID and creation
// time) with the "after" of the last recorded key event, and writes keys.observed when they differ,
// also on a first start with nothing recorded yet, an empty key set included. It runs in one
// transaction under an advisory lock, and loads the key set and the generated Secrets inside it, so
// replicas that start together write once, and a key command that holds the lock from before its
// Secret update to its event (Baseline) is never seen half way. An unchanged state writes nothing.
// load returns the loaded key set (the zero KeySet when koinon-api has none). The actor is a.Actor,
// or koinon-api:startup when it is unset. A write the database refuses for lack of privilege returns
// audit.ErrInstallWriteDenied.
func (a *Auditor) Observe(ctx context.Context, load KeyLoader) (ObserveResult, error) {
	actor := a.Actor
	if actor.Kind == "" {
		actor = ActorStartup
	}
	var res ObserveResult
	wrote := false
	_, err := audit.InPgTx(ctx, a.Pool, func(tx pgx.Tx, b audit.Batch) error {
		res, wrote = ObserveResult{}, false
		if err := lockKeys(ctx, tx); err != nil {
			return err
		}
		r, ks, ev, err := a.compare(ctx, tx, load, actor, true)
		clearKeySet(ks)
		if err != nil {
			return err
		}
		res = r
		if ev != nil {
			b.Install(*ev)
			wrote = true
		}
		return nil
	})
	if err == nil {
		res.Wrote = wrote
	}
	return res, err
}

// Change is one key-set change in progress: a transaction that holds the advisory lock of the key
// events from before the change is made to after its event is written. A start-up check (Observe) that
// runs meanwhile waits for it, so it never sees the changed Secret without the event that records it.
// The command makes its change, writes its events with Record and ends with Commit. A nil Change records
// nothing.
type Change struct {
	a       *Auditor
	tx      pgx.Tx
	command string
	// Before is the key-set state the change starts from.
	Before KeyState
	// pending is the keys.observed of a change whose event was lost, written before the command's own.
	pending *audit.InstallEvent
	done    bool
}

// Baseline begins a key command: it takes the advisory lock, loads the live key set with load and
// returns it with the Change that keeps the lock. The command edits that key set, so what it changes
// is what the lock covers, saves it and records its event in the Change. A key change whose event was
// lost (its Secret was saved, its write failed) leaves the live KEKs different from the last recorded
// state; the Change then writes keys.observed first, as the actor of the command itself (the CLI's, or
// the Job's), so the command's own event starts from what was recorded and the earlier change is not
// absorbed into it. The generated Secrets are not compared: a writer that cannot list them merges the
// recorded ones, and the start-up check owns them. With no earlier event there is nothing to compare.
// command names the keys subcommand for a CLI actor. An error of load is a *KeyLoadError. The caller
// clears the returned key set and always ends the Change (Commit).
func (a *Auditor) Baseline(ctx context.Context, load KeyLoader, command string) (*Change, KeySet, error) {
	type begun struct {
		c  *Change
		ks KeySet
	}
	b, err := retryChain(ctx, a.budget(), func(ctx context.Context) (begun, error) {
		tx, err := a.Pool.Begin(ctx)
		if err != nil {
			return begun{}, err
		}
		if err := lockKeys(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
			return begun{}, err
		}
		res, ks, ev, err := a.compare(ctx, tx, load, a.actor(command), false)
		if err != nil {
			_ = tx.Rollback(ctx)
			return begun{}, err
		}
		return begun{c: &Change{a: a, tx: tx, command: command, Before: res.After, pending: ev}, ks: ks}, nil
	})
	if err != nil {
		return nil, KeySet{}, err
	}
	return b.c, b.ks, nil
}

// After describes ks, as loaded again after the change, relative to the change's "before".
func (c *Change) After(ctx context.Context, ks KeySet) (KeyState, error) {
	if c == nil {
		return KeyState{}, nil
	}
	return c.a.After(ctx, ks, c.Before)
}

// insert writes evs in the Change's transaction, all or none, in a savepoint so a refused insert can
// be retried, for the Auditor's Wait while the chain cannot take them yet. With hook, the Auditor's
// Write replaces the insert.
func (c *Change) insert(ctx context.Context, hook bool, evs ...audit.InstallEvent) error {
	_, err := retryChain(ctx, c.a.budget(), func(ctx context.Context) (struct{}, error) {
		if hook && c.a.Write != nil {
			for _, ev := range evs {
				if err := c.a.Write(ctx, ev); err != nil {
					return struct{}{}, fmt.Errorf("write the %s install audit event: %w", ev.Action, err)
				}
			}
			return struct{}{}, nil
		}
		sp, err := c.tx.Begin(ctx)
		if err != nil {
			return struct{}{}, err
		}
		for _, ev := range evs {
			if _, err := audit.WriteInTx(ctx, sp, func(b audit.Batch) { b.Install(ev) }); err != nil {
				_ = sp.Rollback(ctx)
				return struct{}{}, fmt.Errorf("write the %s install audit event: %w", ev.Action, err)
			}
		}
		return struct{}{}, sp.Commit(ctx)
	})
	return err
}

// flush writes the pending keys.observed, once.
func (c *Change) flush(ctx context.Context) error {
	if c.pending == nil {
		return nil
	}
	if err := c.insert(ctx, false, *c.pending); err != nil {
		return err
	}
	c.pending = nil
	return nil
}

// Record writes es in the Change's transaction, all or none, after the keys.observed of a lost change
// if there is one. They are visible once Commit has run.
func (c *Change) Record(ctx context.Context, es ...Event) error {
	if c == nil {
		return nil
	}
	if err := c.flush(ctx); err != nil {
		return err
	}
	evs := make([]audit.InstallEvent, 0, len(es))
	for _, e := range es {
		command := e.Command
		if command == "" {
			command = c.command
		}
		evs = append(evs, audit.InstallEvent{Actor: c.a.actor(command), Action: e.Action, TargetKind: e.TargetKind, TargetID: e.TargetID, Before: e.Before, After: e.After})
	}
	return c.insert(ctx, true, evs...)
}

// Commit writes the pending keys.observed (a change that failed still records the lost one it found),
// commits and releases the lock. It runs once; later calls, and calls on a nil Change, do nothing.
func (c *Change) Commit(ctx context.Context) error {
	if c == nil || c.done {
		return nil
	}
	c.done = true
	if err := c.flush(ctx); err != nil {
		_ = c.tx.Rollback(ctx)
		return err
	}
	return c.tx.Commit(ctx)
}
