// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/keyscli"
)

// keyAudit is koinon-api's own writer of key events (ADR 0100 decision 3): the keys it generates on
// start, and the start-up check that catches an event that was lost. It sees every generated Secret:
// the ones labelled keyscli.GeneratedLabel, the KEK Secret and the saved-work key.
type keyAudit struct {
	pool *pgxpool.Pool
	// keySet is the KEK store (the Secret or the mounted directory); nil when koinon-api has no KEK.
	keySet keyscli.Store
	// generated reads the generated Secrets' metadata; nil without Kubernetes access.
	generated func(ctx context.Context) ([]keyscli.GeneratedSecret, error)
	// kekSecret is the name of the KEK Secret; the KEK part of a keys.generated event for any other
	// Secret comes from the last recorded state, not from the live key set.
	kekSecret string

	kube      kubernetes.Interface
	namespace string
}

func newKeyAudit(pool *pgxpool.Pool, kube kubernetes.Interface, namespace, kekSecret string, secretNames ...string) *keyAudit {
	k := &keyAudit{pool: pool, kube: kube, namespace: namespace, kekSecret: kekSecret}
	if kube != nil {
		k.generated = keyscli.GeneratedReader{Client: kube, Namespace: namespace, ListLabelled: true, Names: append([]string{kekSecret}, secretNames...)}.Read
	}
	return k
}

func (k *keyAudit) auditor(actor audit.Actor) *keyscli.Auditor {
	return &keyscli.Auditor{Pool: k.pool, Actor: actor, Generated: k.generated, Authoritative: true}
}

func (k *keyAudit) load(ctx context.Context) (keyscli.KeySet, error) {
	if k.keySet == nil {
		return keyscli.KeySet{}, nil
	}
	return k.keySet.Load(ctx)
}

func clearKeys(ks keyscli.KeySet) {
	for _, key := range ks.Keys {
		clear(key)
	}
}

// generatedEvent records a key koinon-api has just created (keys.generated, actor koinon-api:keys).
// Its "after" changes only the Secret created, on top of the last recorded state (the KEK part is the
// live key set only when the created Secret is the KEK Secret). Any other difference, such as the
// ensure-secrets Secrets or a key event that was lost, is left for the start-up check to write as
// keys.observed. The creation is not atomic with the event; if the write fails the start-up check
// records the key set as keys.observed, so a failure is logged, not fatal.
func (k *keyAudit) generatedEvent(ctx context.Context, kind, id string) {
	err := func() error {
		ks, err := k.load(ctx)
		if err != nil {
			return err
		}
		defer clearKeys(ks)
		a := &keyscli.Auditor{Pool: k.pool, Actor: keyscli.ActorKeys}
		if k.kube != nil {
			a.Generated = keyscli.GeneratedReader{Client: k.kube, Namespace: k.namespace, Names: []string{id}}.Read
		}
		prev, err := a.LastState(ctx)
		if err != nil {
			return err
		}
		after, err := a.Snapshot(ctx, ks)
		if err != nil {
			return err
		}
		if id != k.kekSecret {
			// Not the KEK Secret: the KEK part is the last recorded one (empty on a first start), so a KEK
			// from an existing Secret or a mounted directory is left for keys.observed.
			var p keyscli.KeyState
			if prev != nil {
				p = *prev
			}
			after.KEKs, after.Active, after.Retired = p.KEKs, p.Active, p.Retired
		}
		after = after.Normalized()
		return a.Record(ctx, keyscli.Event{Action: keyscli.ActionGenerated, TargetKind: kind, TargetID: id, After: after})
	}()
	if err != nil {
		slog.Error("the generated key is not recorded in the install audit log; the start-up check records the key set as keys.observed",
			"secret_name", id, "err", err)
	}
}

// ensureGenerated runs create, which makes a key Secret koinon-api generates (the first KEK, the
// saved-work key) and reports whether it did, and records keys.generated for it as actor koinon-api:keys.
// It does so in a key Change: the key-set advisory lock is taken before create and held through the
// Secret creation and the event, so another replica's start-up check waits and then finds the key
// recorded, instead of writing it as keys.observed first (ADR 0100 decision 3, issue #377).
//
// Start never blocks on the database for this: when the Change cannot begin (the chain is not ready, the
// lock is not free within installWriteFirstAttempt, the key set does not load), create runs without the
// lock and the event is written after it by generatedEvent, as before; the start-up check then catches
// anything that was lost.
func (k *keyAudit) ensureGenerated(ctx context.Context, kind, id string, create func(context.Context) (bool, error)) (bool, error) {
	c := k.beginChange(ctx, id)
	if c == nil {
		created, err := create(ctx)
		if err == nil && created {
			k.generatedEvent(ctx, kind, id)
		}
		return created, err
	}
	created, err := create(ctx)
	if err == nil && created {
		if rerr := k.recordGenerated(ctx, c, kind, id); rerr != nil {
			slog.Error("the generated key is not recorded in the install audit log; the start-up check records the key set as keys.observed",
				"secret_name", id, "err", rerr)
		}
	}
	if cerr := c.Commit(ctx); cerr != nil && err == nil && created {
		slog.Error("the generated key's audit event did not commit; the start-up check records the key set as keys.observed",
			"secret_name", id, "err", cerr)
	}
	return created, err
}

// beginChange takes the key-set lock for one change, or returns nil when it cannot do so at once.
func (k *keyAudit) beginChange(ctx context.Context, id string) *keyscli.Change {
	bctx, cancel := context.WithTimeout(ctx, installWriteFirstAttempt)
	defer cancel()
	// A KEK Secret that does not exist yet is the empty key set: this change makes it.
	load := func(ctx context.Context) (keyscli.KeySet, error) {
		ks, err := k.load(ctx)
		if apierrors.IsNotFound(err) {
			return keyscli.KeySet{}, nil
		}
		return ks, err
	}
	a := &keyscli.Auditor{Pool: k.pool, Actor: keyscli.ActorKeys}
	if k.kube != nil {
		// Only the Secret this change makes is listed; the rest is merged from the last recorded state.
		a.Generated = keyscli.GeneratedReader{Client: k.kube, Namespace: k.namespace, Names: []string{id}}.Read
	}
	c, ks, err := a.Baseline(bctx, load, "")
	clearKeys(ks)
	if err != nil {
		slog.Warn("the key-set lock is not taken for the generated key; its event is written after it", "err", err)
		return nil
	}
	return c
}

// recordGenerated writes keys.generated for the Secret id in c: the state of the key set after the
// creation, on top of the last recorded one (see generatedEvent for what "after" holds).
func (k *keyAudit) recordGenerated(ctx context.Context, c *keyscli.Change, kind, id string) error {
	ks, err := k.load(ctx)
	if err != nil {
		return err
	}
	defer clearKeys(ks)
	prev, err := (&keyscli.Auditor{Pool: k.pool}).LastState(ctx)
	if err != nil {
		return err
	}
	after, err := c.After(ctx, ks)
	if err != nil {
		return err
	}
	if id != k.kekSecret {
		// Not the KEK Secret: the KEK part is the last recorded one (empty on a first start).
		var p keyscli.KeyState
		if prev != nil {
			p = *prev
		}
		after.KEKs, after.Active, after.Retired = p.KEKs, p.Active, p.Retired
	}
	return c.Record(ctx, keyscli.Event{Action: keyscli.ActionGenerated, TargetKind: kind, TargetID: id, After: after.Normalized()})
}

// observe is the start-up check: keys.observed when the key-set state differs from the last recorded.
func (k *keyAudit) observe(ctx context.Context) error {
	return k.observeAs(ctx, keyscli.ActorStartup)
}

// observeAs runs the check (Auditor.Observe) as actor.
func (k *keyAudit) observeAs(ctx context.Context, actor audit.Actor) error {
	// The key set and the generated Secrets are loaded inside Observe, under its advisory lock.
	res, err := k.auditor(actor).Observe(ctx, k.load)
	if err != nil {
		return err
	}
	if res.Wrote {
		slog.Warn("the key set differs from the last recorded key event; recorded keys.observed",
			"actor", actor.ID, "active_kek", res.After.Active, "keks", len(res.After.KEKs), "retired", len(res.After.Retired), "generated_secrets", len(res.After.Generated))
	}
	return nil
}

// watchChanged is the lost-change check of a running koinon-api: the keyring watch found a key set it
// had not seen (watchKeyring). It runs the start-up check again, with the same loader and under the same
// lock, so a key command whose event was lost is recorded without waiting for the next start. A key
// command that is still between its Secret update and its event holds the lock, so the check waits for
// it and finds the change recorded. The actor is koinon-api:keyring-watch, not a start that did not
// happen. A failure is logged; the next change or start checks again.
func (k *keyAudit) watchChanged(ctx context.Context) {
	if err := k.observeAs(ctx, keyscli.ActorKeyringWatch); err != nil {
		slog.Warn("the key-set check after a keyring change did not record its state", "err", err)
	}
}

// startObserving runs the check once (for at most installWriteFirstAttempt), and again in the background every installWriteRetry until it
// succeeds when the chain cannot take the event yet (the CloudNativePG upgrade window, ADR 0100
// decision 1.1a): the start does not wait for it.
func (k *keyAudit) startObserving(ctx context.Context, stop func()) {
	_ = startInstallWrite(ctx, stop, installRetry{Goroutine: "keys-observed-retry", What: "key set check", AnyError: true},
		installWriteRetry, installWriteFirstAttempt, k.observe)
}
