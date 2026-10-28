// SPDX-License-Identifier: Apache-2.0

package pricing

// ListSource supplies the list of price entries that applies when an org has no entry of its own.
// Code that prices requests takes a ListSource instead of calling Default, so a test can give it a
// fixed list and a price update to default.yaml never changes what a test asserts.
//
// A ListSource is meant to be layered. A later source can wrap another one: org overrides first,
// then a snapshot of model metadata, then the embedded default list. Each layer answers
// Current from its own entries followed by those of the source it wraps; Select already prefers
// the more specific and later entry, so a wrapper only has to concatenate. Nothing but the
// embedded list exists today.
type ListSource interface {
	// Current returns the list and its resolved entries. The result is read-only.
	Current() (List, []Resolved)
}

// Embedded returns the ListSource of the default list embedded in the binary: the production wiring.
func Embedded() ListSource { return embedded{} }

type embedded struct{}

func (embedded) Current() (List, []Resolved) { return Default() }

// Static returns a ListSource that always answers with the given list, as ParseList returned it.
// It serves tests that need fixed prices and layers that hold a snapshot.
func Static(l List, rs []Resolved) ListSource { return static{l, rs} }

type static struct {
	l  List
	rs []Resolved
}

func (s static) Current() (List, []Resolved) { return s.l, s.rs }
