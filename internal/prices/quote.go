// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// This file is the one place that prices a usage record (#373). Ledger ingest, the settler and the
// agent daemon's modelFacts ask [Resolver.Find] for the model's price at a time and [Quote.Price]
// for the cost of a usage; none of them calls [pricing.Price.Cost] itself. Every caller that has to
// tell "no price" from "unusable price" (these and the availability checks of the reconciler,
// agentd's model wait and agentmodel) does it with [Unpriceable]. [Lookup] answers only whether an
// enabled model of that name exists, which is not a price decision.

// Quote is the model a request used and the price found for it at the request's time. Price turns
// a usage into a cost. When Find returns an error, Model is still set if the model was found.
type Quote struct {
	Model      Model
	Resolution Resolution
}

// Priced is a usage priced: the cost per bucket and their total.
type Priced struct {
	Breakdown pricing.Breakdown
	Cost      money.USD
}

// Reason says why a usage cannot be priced.
type Reason string

const (
	// ReasonNoPrice: no enabled model has this name, or no price entry matches it.
	ReasonNoPrice Reason = "no_price"
	// ReasonUnusablePrice: a stored price cannot be used (a rate that does not parse, an override
	// that no longer validates, a cost that overflows).
	ReasonUnusablePrice Reason = "unusable_price"
)

// Unpriceable classifies an error of [Resolver.Find] or [Quote.Price]. It reports false for an error
// that is neither, such as a database error, which callers return and retry.
func Unpriceable(err error) (Reason, bool) {
	var badPrice *BadPriceError
	switch {
	case errors.As(err, &badPrice):
		return ReasonUnusablePrice, true
	case errors.Is(err, ErrUnpriced):
		return ReasonNoPrice, true
	}
	return "", false
}

// Find looks the enabled model called name up and finds its price at time at. The error is
// [ErrUnpriced] (no model or no price: [ReasonNoPrice]), a *[BadPriceError] ([ReasonUnusablePrice])
// or a database error.
func (rv Resolver) Find(ctx context.Context, q *store.Queries, org pgtype.UUID, name string, at time.Time) (Quote, error) {
	m, err := Lookup(ctx, q, org, name)
	if err != nil {
		return Quote{}, err
	}
	res, err := rv.Resolve(ctx, q, org, m, at)
	if err != nil {
		return Quote{Model: m}, err
	}
	return Quote{Model: m, Resolution: res}, nil
}

// Price prices u at the quote's price. A cost the price cannot compute is a *[BadPriceError].
func (qt Quote) Price(u pricing.Usage) (Priced, error) {
	b, total, err := qt.Resolution.Price.Cost(u)
	if err != nil {
		return Priced{}, &BadPriceError{Err: err}
	}
	return Priced{Breakdown: b, Cost: total}, nil
}
