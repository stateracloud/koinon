// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/safego"
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
}

func newKeyAudit(pool *pgxpool.Pool, kube kubernetes.Interface, namespace string, secretNames ...string) *keyAudit {
	k := &keyAudit{pool: pool}
	if kube != nil {
		k.generated = keyscli.GeneratedReader{Client: kube, Namespace: namespace, ListLabelled: true, Names: secretNames}.Read
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

// generatedEvent records a key koinon-api has just created (keys.generated, actor koinon-api:keys):
// the whole key-set state afterwards. The creation is not atomic with the event; if the write fails
// the start-up check records the key set as keys.observed, so a failure is logged, not fatal.
func (k *keyAudit) generatedEvent(ctx context.Context, kind, id string) {
	err := func() error {
		ks, err := k.load(ctx)
		if err != nil {
			return err
		}
		defer clearKeys(ks)
		a := k.auditor(keyscli.ActorKeys)
		after, err := a.Snapshot(ctx, ks)
		if err != nil {
			return err
		}
		return a.Record(ctx, keyscli.Event{Action: keyscli.ActionGenerated, TargetKind: kind, TargetID: id, After: after})
	}()
	if err != nil {
		slog.Error("the generated key is not recorded in the install audit log; the start-up check records the key set as keys.observed",
			"secret_name", id, "err", err)
	}
}

// observe is the start-up check: keys.observed when the key-set state differs from the last recorded.
func (k *keyAudit) observe(ctx context.Context) error {
	ks, err := k.load(ctx)
	if err != nil {
		return fmt.Errorf("load the key set to observe it: %w", err)
	}
	defer clearKeys(ks)
	res, err := k.auditor(keyscli.ActorStartup).Observe(ctx, ks)
	if err != nil {
		return err
	}
	if res.Wrote {
		slog.Warn("the key set differs from the last recorded key event; recorded keys.observed",
			"active_kek", res.After.Active, "keks", len(res.After.KEKs), "retired", len(res.After.Retired), "generated_secrets", len(res.After.Generated))
	}
	return nil
}

// startObserving runs the check once, and again in the background every installWriteRetry until it
// succeeds when the chain cannot take the event yet (the CloudNativePG upgrade window, ADR 0100
// decision 1.1a): the start does not wait for it.
func (k *keyAudit) startObserving(ctx context.Context, stop func()) {
	err := k.observe(ctx)
	if err == nil {
		return
	}
	slog.Error("the key-set check did not record its state; retrying", "err", err)
	safego.GoOrStop(ctx, slog.Default(), "keys-observed-retry", stop, func(ctx context.Context) {
		retryInstallWrite(ctx, "key set check", installWriteRetry, k.observe)
	})
}
