// SPDX-License-Identifier: Apache-2.0

package budget_test

import (
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/money"
)

func lv(name, limit, spent string) budget.Level {
	return budget.Level{Name: name, Limit: money.MustParse(limit), Spent: money.MustParse(spent)}
}

func TestNetIsTheSmallestRemaining(t *testing.T) {
	st := budget.Status{Levels: []budget.Level{
		lv(budget.AgentDaily, "10", "2.5"),      // 7.5 left
		lv(budget.OrgMonthly, "100", "97"),      // 3 left
		lv(budget.PlatformCeiling, "500", "97"), // 403 left
	}}
	net, binding, ok := st.Net()
	if !ok || net != money.MustParse("3") || binding != budget.OrgMonthly {
		t.Fatalf("%v %v %v", net, binding, ok)
	}
	// Overspent levels never go negative.
	st.Levels[1] = lv(budget.OrgMonthly, "100", "100.004")
	if net, binding, _ := st.Net(); net != 0 || binding != budget.OrgMonthly {
		t.Fatalf("%v %v", net, binding)
	}
	if _, _, ok := (budget.Status{}).Net(); ok {
		t.Fatal("no levels must mean no limit")
	}
}

func TestWindows(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 4, 5, 0, time.UTC)
	if got := budget.DayStart(now); !got.Equal(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if got := budget.MonthStart(now); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	// A local-time input is read as UTC.
	loc := time.FixedZone("x", 14*3600)
	if got := budget.MonthStart(time.Date(2026, 11, 1, 5, 0, 0, 0, loc)); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month start of 2026-11-01 05:00+14 is %v", got)
	}
}

func TestGenerationChangesWithAnySpendButNotWithTime(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	a := budget.Status{Levels: []budget.Level{lv(budget.AgentDaily, "10", "1"), lv(budget.OrgMonthly, "100", "50")}}
	b := budget.Status{Levels: []budget.Level{lv(budget.AgentDaily, "10", "1"), lv(budget.OrgMonthly, "100", "50.000000000001")}}
	if a.Generation(now) == b.Generation(now) {
		t.Fatal("another agent's spend must start a fresh gateway counter")
	}
	if a.Generation(now) != a.Generation(now.Add(time.Hour)) {
		t.Fatal("same day and spend must give the same budget, so re-pushing is a no-op")
	}
	if a.Generation(now) == a.Generation(now.Add(24*time.Hour)) {
		t.Fatal("a new day is a new counter")
	}
	l := gateway.USDLimit(money.MustParse("7.5"), budget.DayWindow, a.Generation(now))
	var s gateway.State
	s.SetLimit(gateway.AgentRef{Org: "acme", Name: "a"}, &l)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReachedAndFraction(t *testing.T) {
	if l := lv("x", "1", "0.99999999"); l.Reached() || l.Fraction() >= 1 {
		t.Fatal("below")
	}
	if !lv("x", "1", "1").Reached() {
		t.Fatal("at the limit counts as reached")
	}
}

func TestPickNamesTheBroadestLimitReachedInItsCurrentWindow(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	day, month := budget.DayStart(now), budget.MonthStart(now)
	at := func(l budget.Level, start time.Time) budget.Level { l.WindowStart = start; return l }
	agent := at(lv(budget.AgentDaily, "2.75", "2.91"), day)
	org := at(lv(budget.OrgMonthly, "3.50", "3.60"), month)
	ceiling := at(lv(budget.PlatformCeiling, "3.55", "3.60"), month)

	for _, c := range []struct {
		name   string
		levels []budget.Level
		want   string
	}{
		{"agent only", []budget.Level{agent, at(lv(budget.OrgMonthly, "10", "3.60"), month)}, budget.AgentDaily},
		{"org over agent", []budget.Level{agent, org}, budget.OrgMonthly},
		{"ceiling over org and agent, whatever the order", []budget.Level{agent, org, ceiling}, budget.PlatformCeiling},
		{"ceiling first in the list", []budget.Level{ceiling, agent}, budget.PlatformCeiling},
		// Last month's cap reached by a late entry names nothing this month.
		{"stale window ignored", []budget.Level{agent, at(lv(budget.OrgMonthly, "3.50", "3.60"), month.AddDate(0, -1, 0))}, budget.AgentDaily},
	} {
		got, ok := budget.Status{Levels: c.levels}.Pick(now)
		if !ok || got.Name != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, got.Name, ok, c.want)
		}
	}
	if _, ok := (budget.Status{Levels: []budget.Level{at(lv(budget.AgentDaily, "10", "1"), day)}}).Pick(now); ok {
		t.Error("nothing reached: nothing picked")
	}
	if _, ok := (budget.Status{Levels: []budget.Level{at(lv(budget.AgentDaily, "1", "2"), day.AddDate(0, 0, -1))}}).Pick(now); ok {
		t.Error("yesterday's limit: nothing picked")
	}
}
