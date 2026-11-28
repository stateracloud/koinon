// SPDX-License-Identifier: Apache-2.0

// Package usage holds the pure part of the usage and cost read model (issue #1268): the exact sum of
// ledger rows, the period of a query and the CSV export. The API and the export build their numbers
// from the same Sum, so they cannot differ.
//
// The sums use exact decimals, not [money.USD]: that type saturates at its largest budget, and a
// total over many orgs and days may be larger.
package usage

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// MaxDays is the longest period of one query.
const MaxDays = 366

// ErrRange means the period is not valid.
var ErrRange = errors.New("usage: invalid period")

// Resolve returns the period of a query: the UTC days from from up to, not including, to. Zero
// times mean the default, the month to date (the first of the month up to the end of today). Both
// ends round down to the start of their UTC day.
func Resolve(from, to, now time.Time) (time.Time, time.Time, error) {
	day := func(t time.Time) time.Time {
		t = t.UTC()
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	today := day(now)
	if from.IsZero() && to.IsZero() {
		return time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC), today.AddDate(0, 0, 1), nil
	}
	if to.IsZero() {
		to = today.AddDate(0, 0, 1)
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	from, to = day(from), day(to)
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: to must be after from", ErrRange)
	}
	if to.Sub(from) > MaxDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: at most %d days", ErrRange, MaxDays)
	}
	return from, to, nil
}

// Row is one group of ledger entries as the database sums it. Cost and Estimated are decimal text.
type Row struct {
	Requests, Input, Output, CacheRead, CacheWrite, Reasoning int64
	Cost, Estimated                                           string
}

// Sum adds rows exactly.
type Sum struct {
	Requests, Input, Output, CacheRead, CacheWrite, Reasoning int64
	cost, estimated                                           big.Rat
}

// Add adds one row. It fails on a cost that is not a decimal.
func (s *Sum) Add(r Row) error {
	var c, e big.Rat
	if _, ok := c.SetString(orZero(r.Cost)); !ok {
		return fmt.Errorf("usage: invalid cost %q", r.Cost)
	}
	if _, ok := e.SetString(orZero(r.Estimated)); !ok {
		return fmt.Errorf("usage: invalid estimated cost %q", r.Estimated)
	}
	s.Requests += r.Requests
	s.Input += r.Input
	s.Output += r.Output
	s.CacheRead += r.CacheRead
	s.CacheWrite += r.CacheWrite
	s.Reasoning += r.Reasoning
	s.cost.Add(&s.cost, &c)
	s.estimated.Add(&s.estimated, &e)
	return nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// Cost is the sum of the costs as decimal text.
func (s *Sum) Cost() string { return Decimal(&s.cost) }

// Estimated is the sum of the estimated costs as decimal text.
func (s *Sum) Estimated() string { return Decimal(&s.estimated) }

// CacheReadShare is the cache read tokens over the whole input side (input, cache read and cache
// write tokens): 0 to 1, and 0 when there is none.
func (s *Sum) CacheReadShare() float64 {
	denom := s.Input + s.CacheRead + s.CacheWrite
	if denom <= 0 {
		return 0
	}
	return float64(s.CacheRead) / float64(denom)
}

// Decimal writes an amount with at most 12 fraction digits, trimming trailing zeros but keeping two
// (the way [money.USD] prints).
func Decimal(r *big.Rat) string {
	s := r.FloatString(12)
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return whole + "." + frac
}
