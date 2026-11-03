// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// KeyAdmin is what the key-admin Job does (ADR 0085 decision 12): it is the only identity that wraps
// a new KEK with the KMS, so it creates the first KEK of an install that opted in to a KMS, and adds
// the KEK an operator asked for (rotation, the move from local KEKs). It runs on every install and
// upgrade and is idempotent: a KEK that is already there is left alone, so a rerun changes nothing.
// It never activates, retires or rewraps values: koinon-api does (keys activate, rewrap, retire). It
// binds the KEKs it wraps to the install (#615) and, when asked, wraps unbound ones again, bound.
type KeyAdmin struct {
	// Store is the KEK Secret, with key-admin wrappers (secretstore.NewKeyAdminWrappers).
	Store KubeStore
	// Create makes the KEK Secret with one active KMS-wrapped KEK when it does not exist (as
	// koinon-api does for a local install; the chart sets it unless the Secret is the operator's).
	Create bool
	// AddID is a KEK to add, wrapped by the KMS, unless the key set holds it already.
	AddID string
	// RewrapUnbound wraps again each KEK of the primary wrapping key that is not bound to the install
	// (stored format 1, from before #615), bound this time. The KEK itself, and so its fingerprint
	// and every value it wraps, stays the same. A bound KEK is left alone, so a rerun wraps nothing.
	RewrapUnbound bool
	// HasValues tells whether the database holds encrypted values: a missing Secret is then
	// restored from backup, never created (KubeStore.Ensure).
	HasValues func(context.Context) (bool, error)
	Out       io.Writer
	Now       func() time.Time
}

// ErrLocalKeyAdmin means the key-admin Job ran without a KMS: a local install adds KEKs from
// koinon-api and has no Job.
var ErrLocalKeyAdmin = errors.New("keyscli: the key-admin Job wraps KEKs with a KMS, and the configured wrapping is local")

// Run does what a is configured for.
func (a KeyAdmin) Run(ctx context.Context) error {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	primary := a.Store.Wrappers.Primary()
	if primary.Backend() == secretstore.BackendLocal {
		return ErrLocalKeyAdmin
	}
	wrappedBy := primary.Backend() + ":" + primary.KeyID()
	if a.Create {
		created, err := a.Store.Ensure(ctx, now(), a.HasValues)
		if err != nil {
			return err
		}
		if created {
			_, _ = fmt.Fprintf(a.Out, "Created the KEK Secret %s with one active KEK wrapped by %s. Back it up now (koinon-api keys export).\n", a.Store.Name, wrappedBy)
		}
	}
	if a.AddID == "" {
		_, _ = fmt.Fprintln(a.Out, "No KEK to add.")
	} else if err := a.add(ctx, now, primary); err != nil {
		return err
	}
	if a.RewrapUnbound {
		return a.rewrapUnbound(ctx, primary)
	}
	return nil
}

// add adds the KEK AddID names, wrapped by primary, unless the key set holds it.
func (a KeyAdmin) add(ctx context.Context, now func() time.Time, primary secretstore.Wrapper) error {
	wrappedBy := primary.Backend() + ":" + primary.KeyID()
	if !secretstore.ValidKEKID(a.AddID) {
		return fmt.Errorf("%q is not a valid KEK ID", a.AddID)
	}
	// The stored form tells whether the KEK is there without unwrapping the key set first, so a KEK
	// that is already there is reported even while an older wrapping key cannot be reached.
	ks, err := a.Store.Load(ctx)
	if s, ok := ks.Stored[a.AddID]; ok {
		if s.Backend != primary.Backend() || s.KeyID != primary.KeyID() {
			return fmt.Errorf("KEK %s exists, wrapped by %s, not by %s; IDs are never reused: pick a new ID", a.AddID, s.String(), wrappedBy)
		}
		_, _ = fmt.Fprintf(a.Out, "KEK %s is already in the key set, wrapped by %s.\n", a.AddID, wrappedBy)
		return nil
	}
	if err != nil {
		return err
	}
	t := &Tool{Store: a.Store, Out: io.Discard, Wrappers: a.Store.Wrappers, Now: now}
	if err := t.Add(ctx, a.AddID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "Added KEK %s, wrapped by %s. Back up the KEK Secret, then in koinon-api: keys activate %s, keys rewrap.\n", a.AddID, wrappedBy, a.AddID)
	return nil
}

// rewrapUnbound wraps again, bound to the install, every KEK of primary that is not bound yet. A KEK
// of another wrapping key is left as it is: the Job may not encrypt with that key, and moving a KEK
// to another wrapping key is rotation's job (keys add, activate, rewrap, retire).
func (a KeyAdmin) rewrapUnbound(ctx context.Context, primary secretstore.Wrapper) error {
	ks, err := a.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("bind the KEKs to the install: %w", err)
	}
	defer func() {
		for _, k := range ks.Keys {
			clear(k)
		}
	}()
	wrappedBy := primary.Backend() + ":" + primary.KeyID()
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(ks.Stored)) {
		s := ks.Stored[id]
		switch {
		case s.Backend == secretstore.BackendLocal || s.Bound():
		case s.Backend != primary.Backend() || s.KeyID != primary.KeyID():
			_, _ = fmt.Fprintf(a.Out, "KEK %s is wrapped by %s, not by %s, and stays unbound: rotate it out (keys add, activate, rewrap, retire).\n", id, s.String(), wrappedBy)
		default:
			delete(ks.Stored, id) // Save wraps a KEK without a stored form with the primary, bound.
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		_, _ = fmt.Fprintf(a.Out, "No unbound KEK of %s to bind.\n", wrappedBy)
		return nil
	}
	if err := a.Store.Save(ctx, ks); err != nil {
		return fmt.Errorf("bind the KEKs to the install: %w", err)
	}
	_, _ = fmt.Fprintf(a.Out, "Bound %s to the install ID (wrapped again by %s; the KEKs are unchanged). Back up the KEK Secret now (koinon-api keys export).\n", strings.Join(ids, ", "), wrappedBy)
	return nil
}
