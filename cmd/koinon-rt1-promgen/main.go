// SPDX-License-Identifier: Apache-2.0

// Command koinon-rt1-promgen writes synthetic LLM spend history for the RT1 fixture Prometheus as
// OpenMetrics text, ready for "promtool tsdb create-blocks-from openmetrics". The output is
// deterministic for a seed and an end time. One team's cost for one model doubles yesterday (see
// anomalyTeam and anomalyModel).
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func main() {
	seed := flag.Uint64("seed", 1, "seed for the jitter")
	endFlag := flag.String("end", "", "time of the last sample, RFC 3339 (default: now minus -lag)")
	lag := flag.Duration("lag", 3*time.Hour, "how long after the last sample Prometheus starts (promtool cannot backfill the last 3 hours); -end defaults to now minus this")
	days := flag.Int("days", 30, "days of history")
	step := flag.Duration("step", 15*time.Minute, "sample interval")
	outPath := flag.String("o", "", "output file (default: standard output)")
	flag.Parse()

	end := time.Now().Add(-*lag)
	if *endFlag != "" {
		t, err := time.Parse(time.RFC3339, *endFlag)
		if err != nil {
			fail(fmt.Errorf("-end: %w", err))
		}
		end = t
	}
	if *days < 1 || *step < time.Minute {
		fail(fmt.Errorf("-days must be at least 1 and -step at least 1m"))
	}
	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fail(err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	if err := generate(out, config{Seed: *seed, End: end, Lag: *lag, Days: *days, Step: *step}); err != nil {
		fail(err)
	}
}

func fail(err error) {
	slog.Error("koinon-rt1-promgen failed", "err", err)
	os.Exit(1)
}
