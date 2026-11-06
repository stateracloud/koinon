// SPDX-License-Identifier: Apache-2.0

// Package money is Koinon's one money type (issue #8, ADR 0056 amendment 2026-10-24).
//
// A [USD] is an exact integer number of pico-USD (10^-12 USD). A price is written in USD per
// million tokens with at most six fraction digits, so the cost of one token is a whole number
// of pico-USD (a [Rate]) and a request's cost is tokens times rate with no rounding at all.
// The type holds up to about 9.2 million USD, far above any limit Koinon accepts ([MaxLimit]);
// arithmetic is checked and reports overflow instead of wrapping.
//
// Floats are never used for a stored or sent amount: agentgateway's config takes a decimal with at
// most nine fraction digits, which [USD.Floor] and [USD.Fixed] produce exactly; the database stores numeric(24,12) and crosses the Go boundary as text.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// USD is an amount in pico-USD.
type USD int64

// PerUSD is the number of pico-USD in one USD.
const PerUSD = 1_000_000_000_000

// MaxLimit is the largest budget Koinon accepts (1,000,000 USD).
const MaxLimit USD = 1_000_000 * PerUSD

// ErrOverflow means an amount does not fit.
var ErrOverflow = errors.New("money: amount overflows")

var decimalRE = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]{1,12})?$`)

// Parse reads a non-negative decimal such as "10.50" or "0.000123456789" (at most 12 fraction digits).
func Parse(s string) (USD, error) {
	if !decimalRE.MatchString(s) {
		return 0, fmt.Errorf("money: invalid USD amount %q (want a non-negative decimal with at most 12 fraction digits)", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 12-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	if w > math.MaxInt64/PerUSD {
		return 0, ErrOverflow // the whole part times PerUSD would not fit
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	return USD(w*PerUSD + f), nil
}

// MustParse is Parse for constants.
func MustParse(s string) USD {
	u, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// FromRat converts an exact non-negative amount in USD to pico-USD, rounding half up at the
// twelfth fraction digit (a provider's own cost, a gateway's decimal). It fails on a negative
// amount or one that does not fit.
func FromRat(r *big.Rat) (USD, error) {
	if r.Sign() < 0 {
		return 0, fmt.Errorf("money: negative amount %s", r.FloatString(12))
	}
	x := new(big.Rat).Mul(r, big.NewRat(PerUSD, 1))
	n := new(big.Int).Quo(new(big.Int).Add(new(big.Int).Mul(x.Num(), big.NewInt(2)), x.Denom()), new(big.Int).Mul(x.Denom(), big.NewInt(2)))
	if !n.IsInt64() {
		return 0, ErrOverflow
	}
	return USD(n.Int64()), nil
}

// FromCents converts cents (the unit of template limits) to USD.
func FromCents(c int64) USD { return USD(c) * (PerUSD / 100) }

var numericRE = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]+)?$`)

// ParseNumeric reads a Postgres numeric rendered as text, such as "3.0000000000", for a sum read
// from the database. It never fails on size: a value above [MaxLimit] saturates at MaxLimit, which
// is at or above every limit Koinon accepts, so a huge sum cannot make a budget check fail.
// Digits beyond the twelfth fraction digit are ignored (they are zero in numeric(24,12)).
func ParseNumeric(s string) (USD, error) {
	if !numericRE.MatchString(s) {
		return 0, fmt.Errorf("money: invalid numeric %q", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > 7 || (len(whole) == 7 && whole >= "1000000") {
		return MaxLimit, nil
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 12 {
		frac = frac[:12]
	}
	if frac != "" {
		whole += "." + frac
	}
	u, err := Parse(whole)
	if err != nil {
		return 0, err
	}
	return min(u, MaxLimit), nil
}

// String formats the amount exactly, trimming trailing zeros but keeping two fraction digits.
func (u USD) String() string {
	neg := u < 0
	n := int64(u)
	if neg {
		n = -n
	}
	whole, frac := n/PerUSD, n%PerUSD
	f := strings.TrimRight(fmt.Sprintf("%012d", frac), "0")
	for len(f) < 2 {
		f += "0"
	}
	s := fmt.Sprintf("%d.%s", whole, f)
	if neg {
		s = "-" + s
	}
	return s
}

// Numeric formats the amount for a numeric(24,12) column.
func (u USD) Numeric() string {
	neg := u < 0
	n := int64(u)
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d.%012d", n/PerUSD, n%PerUSD)
	if neg {
		s = "-" + s
	}
	return s
}

// MaxFractionDigits is the number of fraction digits a [USD] holds.
const MaxFractionDigits = 12

// Floor rounds a non-negative amount down to a multiple of 10^-digits USD (digits 0 to
// [MaxFractionDigits]). Rounding down never hands out more than the amount.
func (u USD) Floor(digits int) USD {
	digits = max(0, min(digits, MaxFractionDigits))
	unit := USD(1)
	for range MaxFractionDigits - digits {
		unit *= 10
	}
	return u - u%unit
}

// HasFractionDigits reports whether the amount needs no more than digits fraction digits.
func (u USD) HasFractionDigits(digits int) bool { return u.Floor(digits) == u }

// Fixed formats the amount in fixed-point notation, never with an exponent, with at least one and at
// most digits fraction digits (trailing zeros trimmed): "4.982724240" is "4.98272424", 5 is "5.0".
// It is the form a gateway's decimal field takes. The amount must be non-negative.
func (u USD) Fixed(digits int) string {
	digits = max(1, min(digits, MaxFractionDigits))
	n := int64(u.Floor(digits))
	whole, frac := n/PerUSD, fmt.Sprintf("%012d", n%PerUSD)[:digits]
	if frac = strings.TrimRight(frac, "0"); frac == "" {
		frac = "0"
	}
	return fmt.Sprintf("%d.%s", whole, frac)
}

// Add returns u+v or ErrOverflow.
func (u USD) Add(v USD) (USD, error) {
	if (v > 0 && u > math.MaxInt64-v) || (v < 0 && u < math.MinInt64-v) {
		return 0, ErrOverflow
	}
	return u + v, nil
}

// SubFloor returns u-v, or 0 when v exceeds u.
func (u USD) SubFloor(v USD) USD {
	if v >= u {
		return 0
	}
	return u - v
}

// Min returns the smaller amount.
func Min(a, b USD) USD {
	if a < b {
		return a
	}
	return b
}

// Rate is a price per token in pico-USD, from a price per million tokens.
type Rate int64

var rateRE = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6})?$`)

// ParseRate reads a price in USD per million tokens with at most six fraction digits.
func ParseRate(s string) (Rate, error) {
	if !rateRE.MatchString(s) {
		return 0, fmt.Errorf("money: invalid price %q (want USD per million tokens, a decimal with at most 6 fraction digits)", s)
	}
	u, err := Parse(s)
	if err != nil {
		return 0, err
	}
	return Rate(u / 1_000_000), nil // u is pico-USD per million tokens
}

// PerMillion formats the rate as USD per million tokens.
func (r Rate) PerMillion() string {
	whole, frac := int64(r)/1_000_000, int64(r)%1_000_000
	f := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	if f == "" {
		return fmt.Sprintf("%d", whole)
	}
	return fmt.Sprintf("%d.%s", whole, f)
}

// Cost is tokens times rate, exactly.
func (r Rate) Cost(tokens int64) (USD, error) {
	if tokens < 0 || r < 0 {
		return 0, errors.New("money: negative tokens or rate")
	}
	p := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(r)))
	if !p.IsInt64() {
		return 0, ErrOverflow
	}
	return USD(p.Int64()), nil
}

// Ratio returns 100*a/b as a float for display and thresholds (never for charging).
func Ratio(a, b USD) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
