// SPDX-License-Identifier: Apache-2.0

package runlimits_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/templates"
)

func TestCheckRequestsReachesAtTheLimit(t *testing.T) {
	l := runlimits.Limits{MaxModelRequests: 5}
	if _, hit := l.CheckRequests(runlimits.Usage{Requests: 4}); hit {
		t.Fatal("4 of 5 is below the limit")
	}
	r, hit := l.CheckRequests(runlimits.Usage{Requests: 5})
	if !hit || r.Message() != "model request limit reached (5 of 5 requests)" || r.Detail()["limit"] != "requests" {
		t.Fatalf("%v %v", r, hit)
	}
	if _, hit := (runlimits.Limits{}).CheckRequests(runlimits.Usage{Requests: 1 << 40}); hit {
		t.Fatal("a run without a request limit (queued before ADR 0080) has none")
	}
}

func TestNearestPicksTheLimitFurthestThrough(t *testing.T) {
	l := runlimits.Limits{WallClock: 30 * time.Minute, MaxModelRequests: 100, MaxSpend: money.MustParse("2.00")}
	for _, c := range []struct {
		name    string
		u       runlimits.Usage
		elapsed time.Duration
		want    runlimits.Kind
		atLeast float64
	}{
		{"time", runlimits.Usage{Requests: 10, Spend: money.MustParse("0.10")}, 27 * time.Minute, runlimits.KindTime, 0.89},
		{"requests", runlimits.Usage{Requests: 85, Spend: money.MustParse("0.10")}, time.Minute, runlimits.KindRequests, 0.84},
		{"spend", runlimits.Usage{Requests: 10, Spend: money.MustParse("1.70")}, time.Minute, runlimits.KindSpend, 0.84},
	} {
		p, ok := l.Nearest(c.u, c.elapsed)
		if !ok || p.Kind != c.want || p.Fraction < c.atLeast {
			t.Errorf("%s: %+v %v", c.name, p, ok)
		}
	}
	if _, ok := (runlimits.Limits{}).Nearest(runlimits.Usage{Requests: 9}, time.Hour); ok {
		t.Error("no limits, no nearest")
	}
}

func TestUsageSplitsTheInputByType(t *testing.T) {
	u := runlimits.Usage{Input: 180_000, CacheRead: 178_000, CacheWrite: 1_500, Output: 400}
	if u.UncachedInput() != 500 || u.Tokens() != 180_400 {
		t.Fatalf("%d %d", u.UncachedInput(), u.Tokens())
	}
	if (runlimits.Usage{Input: 10, CacheRead: 20}).UncachedInput() != 0 {
		t.Fatal("never negative")
	}
}

func TestOverrideAppliesOnlyItsSetFields(t *testing.T) {
	base := agentconfig.Config{Limits: templates.Limits{Run: templates.RunLimits{
		MaxDuration: templates.Duration{Duration: 20 * time.Minute}, MaxModelRequests: 100, MaxSpendUSD: 75,
	}}}.RunLimits()
	got := runlimits.Override{MaxModelRequests: 40}.Apply(base)
	if got.MaxModelRequests != 40 || got.MaxSpend != money.MustParse("0.75") || got.WallClock != 20*time.Minute {
		t.Fatalf("%+v", got)
	}
	if !(runlimits.Override{}).IsZero() || (runlimits.Override{MaxSpend: 1}).IsZero() {
		t.Fatal("IsZero")
	}
}

func TestVersionsWithoutRunLimitsGetThePlatformDefaults(t *testing.T) {
	got := agentconfig.Config{}.RunLimits()
	if got.WallClock != agentconfig.DefaultMaxWallClock || got.MaxModelRequests != agentconfig.DefaultMaxModelRequests || got.MaxSpend != agentconfig.DefaultMaxSpend {
		t.Fatalf("%+v", got)
	}
}

func TestOfReadsTheLimitsOfAStoredRun(t *testing.T) {
	var spend pgtype.Numeric
	if err := spend.Scan("2.500000000000"); err != nil {
		t.Fatal(err)
	}
	l, err := runlimits.Of(store.Run{
		MaxWallClockSeconds: 1800, ParkedSeconds: 60, MaxModelRequests: pgtype.Int8{Int64: 150, Valid: true}, MaxSpendUsd: spend,
	})
	if err != nil || l.WallClock != 30*time.Minute || l.ParkedTime != time.Minute || l.MaxModelRequests != 150 || l.MaxSpend != money.MustParse("2.5") {
		t.Fatalf("%+v %v", l, err)
	}
	old, err := runlimits.Of(store.Run{MaxWallClockSeconds: 1800})
	if err != nil || old.MaxModelRequests != 0 || old.MaxSpend != 0 {
		t.Fatalf("a run queued before ADR 0080 has no request or spend limit: %+v %v", old, err)
	}
}
