// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"time"
)

// config says what to generate. The output depends only on these fields.
type config struct {
	// Seed makes the jitter deterministic.
	Seed uint64
	// End is the time of the last sample.
	End time.Time
	// Lag is how long after End Prometheus starts; "yesterday" is the UTC day before End+Lag.
	Lag time.Duration
	// Days is how many days of history end at End.
	Days int
	// Step is the sample interval.
	Step time.Duration
}

// The planted anomaly: the support team's spend on claude-sonnet-4 doubles yesterday (a runaway
// summarisation loop, in the story's telling). Tokens double with it, as the price per token is fixed.
const (
	anomalyTeam  = "support"
	anomalyModel = "claude-sonnet-4"
)

// series is one team's use of one model.
type series struct {
	Model, Provider, Team string
	// TokensPerHour is the weekday daytime rate.
	TokensPerHour float64
	// USDPerMTok is the blended price per million tokens.
	USDPerMTok float64
}

var seriesList = []series{
	{"claude-sonnet-4", "anthropic", "support", 900_000, 6},
	{"claude-sonnet-4", "anthropic", "platform", 400_000, 6},
	{"claude-haiku-4", "anthropic", "support", 2_500_000, 1.2},
	{"claude-haiku-4", "anthropic", "search", 4_000_000, 1.2},
	{"gpt-4.1", "openai", "data-science", 700_000, 5},
	{"gpt-4.1", "openai", "search", 300_000, 5},
	{"llama-3.3-70b", "selfhosted", "data-science", 3_000_000, 0.4},
	{"llama-3.3-70b", "selfhosted", "platform", 1_000_000, 0.4},
}

// gpuSeries is GPU use by the self-hosted models' teams.
type gpuSeries struct {
	Team, GPU string
	// HoursPerHour is GPUs busy on average.
	HoursPerHour float64
}

var gpuList = []gpuSeries{
	{"data-science", "a100", 2.5},
	{"platform", "a100", 1.0},
}

// load is the traffic shape in [0.1, 1] for a time: quiet nights and weekends.
func load(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	f := 0.55 + 0.45*math.Sin((h-9)/24*2*math.Pi+math.Pi/2) // peaks near 15:00 UTC
	f = math.Max(f, 0.1)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		f *= 0.4
	}
	return f
}

// inAnomaly reports whether t falls on yesterday for a Prometheus started at start.
func inAnomaly(t, start time.Time) bool {
	today := start.UTC().Truncate(24 * time.Hour)
	return !t.Before(today.AddDate(0, 0, -1)) && t.Before(today)
}

func seedFor(seed uint64, parts ...string) *rand.Rand {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return rand.New(rand.NewPCG(seed, h.Sum64())) //nolint:gosec // deterministic synthetic data, not security
}

// generate writes the OpenMetrics text: counters with _total, timestamps in whole seconds, one TYPE
// line per family, samples grouped by family and ascending in time, and a closing "# EOF".
func generate(out io.Writer, c config) error {
	w := bufio.NewWriter(out)
	first := c.End.UTC().Truncate(c.Step).Add(-time.Duration(c.Days) * 24 * time.Hour)
	last := c.End.UTC().Truncate(c.Step)
	start := last.Add(c.Lag)

	type family struct {
		name, help string
		labels     func(i int) string
		count      int
		rate       func(i int, t time.Time) float64 // increase per hour at t
		format     func(v float64) string
	}
	tok := func(i int) string {
		s := seriesList[i]
		return fmt.Sprintf(`model=%q,provider=%q,team=%q`, s.Model, s.Provider, s.Team)
	}
	boost := func(s series, t time.Time) float64 {
		if s.Team == anomalyTeam && s.Model == anomalyModel && inAnomaly(t, start) {
			return 2
		}
		return 1
	}
	families := []family{
		{
			name: "llm_tokens", help: "Tokens processed by LLM calls (synthetic).", labels: tok, count: len(seriesList),
			rate:   func(i int, t time.Time) float64 { s := seriesList[i]; return s.TokensPerHour * load(t) * boost(s, t) },
			format: func(v float64) string { return strconv.FormatInt(int64(v), 10) },
		},
		{
			name: "llm_cost_usd", help: "Spend on LLM calls in US dollars (synthetic).", labels: tok, count: len(seriesList),
			rate: func(i int, t time.Time) float64 {
				s := seriesList[i]
				return s.TokensPerHour * load(t) * boost(s, t) / 1e6 * s.USDPerMTok
			},
			format: func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) },
		},
		{
			name: "gpu_hours", help: "GPU hours used by self-hosted models (synthetic).",
			labels: func(i int) string { g := gpuList[i]; return fmt.Sprintf(`gpu=%q,team=%q`, g.GPU, g.Team) },
			count:  len(gpuList),
			rate:   func(i int, t time.Time) float64 { return gpuList[i].HoursPerHour * (0.3 + 0.7*load(t)) },
			format: func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) },
		},
	}

	for _, f := range families {
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n# HELP %s %s\n", f.name, f.name, f.help)
		for i := 0; i < f.count; i++ {
			rng := seedFor(c.Seed, f.name, f.labels(i))
			value := math.Floor(rng.Float64() * 1000) // counters do not start at zero
			stepHours := c.Step.Hours()
			for t := first; !t.After(last); t = t.Add(c.Step) {
				_, _ = fmt.Fprintf(w, "%s_total{%s} %s %d\n", f.name, f.labels(i), f.format(value), t.Unix())
				// The increase over the next step, with 10% jitter.
				value += f.rate(i, t) * stepHours * (0.9 + 0.2*rng.Float64())
			}
		}
	}
	_, _ = fmt.Fprintln(w, "# EOF")
	return w.Flush()
}
