// SPDX-License-Identifier: Apache-2.0

package money_test

import (
	"math/big"
	"testing"

	"github.com/stateracloud/koinon/internal/money"
)

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]string{"10": "10.00", "0.5": "0.50", "0.000000000001": "0.000000000001", "3.0000000000": "3.00", "1.234567890123": "1.234567890123"} {
		u, err := money.ParseNumeric(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := u.String(); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "-1", "1e3", "1.1234567890123", "abc", "10000000"} {
		if _, err := money.Parse(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	u := money.MustParse("12.5")
	if u.Numeric() != "12.500000000000" {
		t.Errorf("numeric %s", u.Numeric())
	}
}

func TestRateCostExact(t *testing.T) {
	r, err := money.ParseRate("0.0375")
	if err != nil {
		t.Fatal(err)
	}
	if r.PerMillion() != "0.0375" {
		t.Errorf("per million %s", r.PerMillion())
	}
	c, err := r.Cost(1_000_000)
	if err != nil || c.String() != "0.0375" {
		t.Errorf("cost %v %v", c, err)
	}
	c, _ = r.Cost(1)
	if c.Numeric() != "0.000000037500" {
		t.Errorf("one token %s", c.Numeric())
	}
	if _, err := money.ParseRate("1.1234567"); err == nil {
		t.Error("7 fraction digits accepted")
	}
	if _, err := money.Rate(1 << 62).Cost(1 << 10); err == nil {
		t.Error("overflow not reported")
	}
}

func TestParseRefusesWhatWouldOverflow(t *testing.T) {
	if _, err := money.Parse("9999999"); err == nil {
		t.Fatal("9,999,999 USD times 10^12 overflows int64 and must be refused")
	}
	if _, err := money.Parse("9223372"); err != nil {
		t.Fatal(err)
	}
}

func TestParseNumericSaturatesInsteadOfFailing(t *testing.T) {
	for _, in := range []string{"1000000.000000000000", "99999999999.5", "123456789012345678", "9999999.999999999999"} {
		u, err := money.ParseNumeric(in)
		if err != nil || u != money.MaxLimit {
			t.Errorf("%s: %v %v", in, u, err)
		}
	}
	u, err := money.ParseNumeric("0000.500000000000")
	if err != nil || u.String() != "0.50" {
		t.Errorf("%v %v", u, err)
	}
}

func TestCents(t *testing.T) {
	if money.FromCents(1050).String() != "10.50" {
		t.Fatal(money.FromCents(1050))
	}
	if s := money.MustParse("1").SubFloor(money.MustParse("2")); s != 0 {
		t.Fatal(s)
	}
}

func TestFromRatRoundsHalfUpAtThePicoUSD(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0.0001234", "0.0001234"},
		{"1.234e-4", "0.0001234"},
		{"0.0000000000005", "0.000000000001"},
		{"0.0000000000004", "0.00"},
		{"3", "3.00"},
	} {
		r, _ := new(big.Rat).SetString(c.in)
		got, err := money.FromRat(r)
		if err != nil || got.String() != c.want {
			t.Errorf("%s: %v %v, want %s", c.in, got, err, c.want)
		}
	}
	if _, err := money.FromRat(big.NewRat(-1, 1)); err == nil {
		t.Error("negative accepted")
	}
	if _, err := money.FromRat(new(big.Rat).SetInt64(1 << 62)); err == nil {
		t.Error("overflow accepted")
	}
}
