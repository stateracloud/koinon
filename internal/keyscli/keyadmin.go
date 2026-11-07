// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"crypto/rand"
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
// It never activates (but for ActivateIfNoActive, where there is no active KEK to replace), retires or
// rewraps values: koinon-api does (keys activate, rewrap, retire). It
// binds the KEKs it wraps to the install (koinon-private#615) and, when asked, wraps unbound ones again, bound.
type KeyAdmin struct {
	// Store is the KEK Secret, with key-admin wrappers (secretstore.NewKeyAdminWrappers).
	Store KubeStore
	// Create makes the KEK Secret with one active KMS-wrapped KEK when it does not exist (as
	// koinon-api does for a local install; the chart sets it unless the Secret is the operator's).
	Create bool
	// AddID is a KEK to add, wrapped by the KMS, unless the key set holds it already.
	AddID string
	// ActivateIfNoActive is the recovery from a lost active KEK (issue #255), with AddID: when the
	// KEK Secret's active entry is missing or names a KEK the Secret no longer holds (the KMS key
	// that wrapped it is gone and the operator removed its entry), AddID is added and made active in
	// one write. When the Secret has a working active KEK it changes nothing about activation, so the
	// Job still never replaces an active KEK.
	ActivateIfNoActive bool
	// RewrapUnbound wraps again each KEK of the primary wrapping key that is not bound to the install
	// (stored format 1, from before koinon-private#615), bound this time. The KEK itself, and so its fingerprint
	// and every value it wraps, stays the same. A bound KEK is left alone, so a rerun wraps nothing.
	RewrapUnbound bool
	// HasValues tells whether the database holds encrypted values: a missing Secret is then
	// restored from backup, never created (KubeStore.Ensure).
	HasValues func(context.Context) (bool, error)
	// UsedID tells whether any stored value in the database carries an envelope under the KEK ID
	// (the usage query of keys status and keys forget). The recovery from a lost active KEK refuses
	// an ID it reports as used: the envelopes under it were sealed by another key, so a new KEK
	// under that ID would be tried against them and keys forget would then refuse the ID.
	// Required with ActivateIfNoActive.
	UsedID func(ctx context.Context, id string) (bool, error)
	// Marker is the install's bound-only marker (koinon-private#615). Creating the first KEK sets it (before the
	// Secret exists), and so does RewrapUnbound once no unbound KEK remains. Required with Create
	// or RewrapUnbound.
	Marker BoundOnlyMarker
	Out    io.Writer
	Now    func() time.Time
}

// BoundOnlyMarker reads and sets the install's bound-only marker (installid.Marker). It has no way
// to clear it.
type BoundOnlyMarker interface {
	BoundOnly(ctx context.Context) (secretstore.BoundOnly, error)
	SetBoundOnly(ctx context.Context, how string) (secretstore.BoundOnly, error)
}

// rollbackNote is said whenever the marker is set or about to be.
const rollbackNote = "a rollback below koinon-private#615 no longer works (those releases cannot read bound KEKs). " +
	"If an old KEK Secret backup may have leaked, rotate the KEKs as well (keys add, activate, rewrap, retire): " +
	"only rotation makes an old backup useless everywhere"

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
	if (a.Create || a.RewrapUnbound) && a.Marker == nil {
		return errors.New("keyscli: the key-admin Job needs the bound-only marker (the database) to create or bind KEKs")
	}
	if a.ActivateIfNoActive && a.AddID == "" {
		// Checked before anything is written, so a misconfigured Job changes nothing.
		return errors.New("keyscli: recovering from a lost active KEK needs the ID of the KEK to add")
	}
	if a.ActivateIfNoActive && a.UsedID == nil {
		// Fail closed: without the probe a retired or dead KEK's ID could be reused.
		return errors.New("keyscli: recovering from a lost active KEK needs the database to check that the new KEK ID is unused")
	}
	if a.Create {
		// The first KEK of an install is bound, so the install is bound-only from the start: the
		// marker is set before the Secret exists, and a failure leaves no Secret, so a rerun sets it.
		created, err := a.Store.EnsureWith(ctx, now(), a.HasValues, func(ctx context.Context) error {
			b, err := a.Marker.SetBoundOnly(ctx, secretstore.BoundOnlyFirstInstall)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.Out, "The bound-only marker of this install is %s: KEKs stored unbound never open here.\n", b)
			return nil
		})
		if err != nil {
			return err
		}
		if created {
			_, _ = fmt.Fprintf(a.Out, "Created the KEK Secret %s with one active KEK wrapped by %s. Back it up now (koinon-api keys export).\n", a.Store.Name, wrappedBy)
		}
	}
	if a.AddID == "" {
		_, _ = fmt.Fprintln(a.Out, "No KEK to add.")
	} else if err := a.addOrRecover(ctx, now, primary); err != nil {
		return err
	}
	if a.RewrapUnbound {
		return a.rewrapUnbound(ctx, primary)
	}
	return nil
}

// addOrRecover is add, preceded by the recovery of a lost active KEK when ActivateIfNoActive asks.
func (a KeyAdmin) addOrRecover(ctx context.Context, now func() time.Time, primary secretstore.Wrapper) error {
	if !a.ActivateIfNoActive {
		return a.add(ctx, now, primary)
	}
	done, err := a.recoverActive(ctx, now, primary)
	if err != nil || done {
		return err
	}
	return a.add(ctx, now, primary)
}

// recoverActive adds AddID, wrapped by primary, and makes it active in one write, when the key set
// has no active KEK it can use. It reports whether it did. The KEKs the Secret still holds must
// unwrap: the operator removed the entries of the lost ones first.
func (a KeyAdmin) recoverActive(ctx context.Context, now func() time.Time, primary secretstore.Wrapper) (bool, error) {
	wrappedBy := primary.Backend() + ":" + primary.KeyID()
	if !secretstore.ValidKEKID(a.AddID) {
		return false, fmt.Errorf("%q is not a valid KEK ID", a.AddID)
	}
	ks, err := a.Store.LoadWithoutActive(ctx)
	defer func() {
		for _, k := range ks.Keys {
			clear(k)
		}
	}()
	if err != nil {
		if errors.Is(err, secretstore.ErrOpen) || errors.Is(err, secretstore.ErrUnreachable) {
			return false, fmt.Errorf("recover the active KEK: %w; if the wrapping key of those KEKs is lost for good, remove the entries of the KEKs that cannot be unwrapped from the KEK Secret first (docs/ops/secret-keys.md)", err)
		}
		return false, err
	}
	if _, ok := ks.Stored[ks.Active]; ok && ks.Active != "" {
		_, _ = fmt.Fprintf(a.Out, "The active KEK %s is in the key set; nothing to recover.\n", ks.Active)
		return false, nil
	}
	if _, ok := ks.Stored[a.AddID]; ok {
		return false, fmt.Errorf("KEK %s exists; IDs are never reused: pick a new ID", a.AddID)
	}
	if a.AddID == ks.Active {
		// The lost KEK's entry is gone but envelopes sealed by it still carry its ID.
		return false, fmt.Errorf("KEK %s is the lost active KEK; IDs are never reused: pick a new ID", a.AddID)
	}
	// Any other retired or dead KEK the operator removed from the Secret may still seal values.
	used, err := a.UsedID(ctx, a.AddID)
	if err != nil {
		return false, fmt.Errorf("check that KEK ID %s is unused: %w", a.AddID, err)
	}
	if used {
		return false, fmt.Errorf("KEK %s seals values in the database although the Secret does not hold it; IDs are never reused: pick a new ID", a.AddID)
	}
	key := make([]byte, secretstore.KeySize)
	if _, err := rand.Read(key); err != nil {
		return false, err
	}
	defer clear(key)
	if ks.Keys == nil {
		ks.Keys = map[string][]byte{}
	}
	lost := ks.Active
	ks.Keys[a.AddID], ks.Active, ks.ActivatedAt = key, a.AddID, now()
	if err := a.Store.Save(ctx, ks); err != nil {
		return false, err
	}
	was := "the Secret named none"
	if lost != "" {
		was = "the Secret named " + lost + ", which it does not hold"
	}
	_, _ = fmt.Fprintf(a.Out, "Added KEK %s, wrapped by %s, and made it active (%s). Back up the KEK Secret now (koinon-api keys export), then forget the lost KEKs: keys forget <kek-id>.\n", a.AddID, wrappedBy, was)
	return true, nil
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
	b, err := a.Marker.BoundOnly(ctx)
	if err != nil {
		return fmt.Errorf("bind the KEKs to the install: %w", err)
	}
	if !b.Set {
		_, _ = fmt.Fprintf(a.Out, "Binding the KEKs to the install. Once no KEK is unbound, the bound-only marker is set and KEKs stored unbound never open again: %s.\n", rollbackNote)
	}
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
	} else {
		if err := a.Store.Save(ctx, ks); err != nil {
			return fmt.Errorf("bind the KEKs to the install: %w", err)
		}
		_, _ = fmt.Fprintf(a.Out, "Bound %s to the install ID (wrapped again by %s; the KEKs are unchanged). Back up the KEK Secret now (koinon-api keys export).\n", strings.Join(ids, ", "), wrappedBy)
	}
	return a.markBoundOnly(ctx, b)
}

// markBoundOnly sets the bound-only marker when the KEK Secret, as stored now, holds no unbound
// KEK. It reads the stored forms again rather than trusting what it just saved.
func (a KeyAdmin) markBoundOnly(ctx context.Context, b secretstore.BoundOnly) error {
	if b.Set {
		_, _ = fmt.Fprintf(a.Out, "The bound-only marker of this install is %s.\n", b)
		return nil
	}
	ks, err := a.Store.Load(ctx)
	for _, k := range ks.Keys {
		clear(k)
	}
	if err != nil {
		return fmt.Errorf("check that every KEK is bound: %w", err)
	}
	if left := secretstore.UnboundKEKs(ks.Stored); len(left) > 0 {
		_, _ = fmt.Fprintf(a.Out, "%s stay unbound, so the bound-only marker is not set: rotate them out, then run the Job again.\n", strings.Join(left, ", "))
		return nil
	}
	b, err = a.Marker.SetBoundOnly(ctx, secretstore.BoundOnlyRewrap)
	if err != nil {
		return fmt.Errorf("set the bound-only marker: %w", err)
	}
	_, _ = fmt.Fprintf(a.Out, "Every KEK is bound. The bound-only marker of this install is %s: every Koinon process now refuses KEKs stored unbound, so an old KEK Secret backup no longer opens. Note: %s.\n", b, rollbackNote)
	return nil
}
