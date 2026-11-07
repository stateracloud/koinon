// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// Wait is how long record retries while the chain cannot take the event yet (the table is not
	// migrated, the writer role is not granted): the key-admin Job runs next to the migration Job.
	Wait time.Duration
	// Write replaces the insert, for tests.
	Write func(ctx context.Context, e audit.InstallEvent) error
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
	prev, err := retryChain(ctx, a.Wait, a.LastState)
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
	_, err := retryChain(ctx, a.Wait, func(ctx context.Context) (struct{}, error) {
		if a.Write != nil {
			return struct{}{}, a.Write(ctx, ev)
		}
		return struct{}{}, audit.RecordInstallErr(ctx, store.New(a.Pool), ev)
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

// retryChain runs f, and again every two seconds while the chain is not ready and wait has not passed.
func retryChain[T any](ctx context.Context, wait time.Duration, f func(context.Context) (T, error)) (T, error) {
	deadline := time.Now().Add(wait)
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

// Observe is the start-up check (ADR 0100 decision 3): it compares the whole key-set state (KEK IDs
// and fingerprints, the active KEK, the retired KEKs, the generated Secrets' name, UID and creation
// time) with the "after" of the last recorded key event, and writes keys.observed when they differ.
// It runs in one transaction under an advisory lock, so replicas that start together write once.
// An unchanged state writes nothing. ks is the loaded key set (the zero KeySet when koinon-api has
// none). A write the database refuses for lack of privilege returns audit.ErrInstallWriteDenied.
func (a *Auditor) Observe(ctx context.Context, ks KeySet) (ObserveResult, error) {
	gen, err := a.generated(ctx)
	if err != nil {
		return ObserveResult{}, err
	}
	var res ObserveResult
	err = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryLockName); err != nil {
			return err
		}
		q := store.New(tx)
		prev, err := lastState(ctx, q)
		if err != nil {
			return err
		}
		cur := stateOf(ks, prev, gen, true)
		res = ObserveResult{Before: prev, After: cur}
		if prev != nil && prev.Equal(cur) {
			return nil
		}
		ev := audit.InstallEvent{Actor: ActorStartup, Action: ActionObserved, TargetKind: "keys", TargetID: "keyset", After: cur}
		if prev != nil {
			ev.Before = *prev
		}
		if err := audit.RecordInstallErr(ctx, q, ev); err != nil {
			return err
		}
		res.Wrote = true
		return nil
	})
	return res, err
}
