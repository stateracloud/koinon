// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPromgen(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-rt1-promgen")
}

var sampleLine = regexp.MustCompile(`^([a-z_]+)\{([^}]*)\} ([0-9.]+) ([0-9]+)$`)

type sample struct {
	name, labels string
	value        float64
	ts           int64
}

func parse(out string) []sample {
	var res []sample
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		Expect(m).NotTo(BeNil(), line)
		v, err := strconv.ParseFloat(m[3], 64)
		Expect(err).NotTo(HaveOccurred())
		ts, err := strconv.ParseInt(m[4], 10, 64)
		Expect(err).NotTo(HaveOccurred())
		res = append(res, sample{m[1], m[2], v, ts})
	}
	return res
}

var _ = Describe("generate", func() {
	// Prometheus starts 2026-10-28 08:00 UTC; the data ends 3 h earlier.
	end := time.Date(2026, 10, 28, 5, 0, 0, 0, time.UTC)
	cfg := config{Seed: 7, End: end, Lag: 3 * time.Hour, Days: 30, Step: 15 * time.Minute}
	run := func(c config) string {
		var b bytes.Buffer
		Expect(generate(&b, c)).To(Succeed())
		return b.String()
	}

	It("writes OpenMetrics: TYPE per family, _total counters, seconds, closing EOF", func() {
		out := run(cfg)
		Expect(out).To(HaveSuffix("# EOF\n"))
		Expect(strings.Count(out, "# EOF")).To(Equal(1))
		for _, f := range []string{"llm_tokens", "llm_cost_usd", "gpu_hours"} {
			Expect(strings.Count(out, "# TYPE "+f+" counter\n")).To(Equal(1), f)
		}
		samples := parse(out)
		Expect(samples).NotTo(BeEmpty())
		names := map[string]bool{}
		for _, s := range samples {
			names[s.name] = true
			Expect(s.ts).To(BeNumerically("<=", end.Unix()))
			Expect(s.ts).To(BeNumerically(">=", end.AddDate(0, 0, -30).Unix()))
		}
		Expect(names).To(Equal(map[string]bool{"llm_tokens_total": true, "llm_cost_usd_total": true, "gpu_hours_total": true}))
		Expect(out).To(ContainSubstring(`llm_tokens_total{model="claude-sonnet-4",provider="anthropic",team="support"}`))
	})

	It("keeps each series ascending in time and monotonic", func() {
		prev := map[string]sample{}
		for _, s := range parse(run(cfg)) {
			key := s.name + s.labels
			if p, ok := prev[key]; ok {
				Expect(s.ts).To(BeNumerically(">", p.ts), key)
				Expect(s.value).To(BeNumerically(">=", p.value), key)
			}
			prev[key] = s
		}
	})

	It("is deterministic for a seed and an end time, and varies with the seed", func() {
		Expect(run(cfg)).To(Equal(run(cfg)))
		other := cfg
		other.Seed = 8
		Expect(run(other)).NotTo(Equal(run(cfg)))
	})

	It("does not depend on the wall clock", func() {
		// The same end time a bit later within the same step gives the same file.
		later := cfg
		later.End = end.Add(7 * time.Minute)
		Expect(run(later)).To(Equal(run(cfg)))
	})

	Describe("the planted anomaly", func() {
		// dayIncrease is a counter series' increase over one UTC day.
		dayIncrease := func(samples []sample, name, labels string, day time.Time) float64 {
			var atStart, atEnd *sample
			for i, s := range samples {
				if s.name != name || s.labels != labels {
					continue
				}
				if s.ts == day.Unix() {
					atStart = &samples[i]
				}
				if s.ts == day.AddDate(0, 0, 1).Unix() {
					atEnd = &samples[i]
				}
			}
			Expect(atStart).NotTo(BeNil())
			Expect(atEnd).NotTo(BeNil())
			return atEnd.value - atStart.value
		}
		samples := func() []sample { return parse(run(cfg)) }
		yesterday := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
		const sonnet = `model="claude-sonnet-4",provider="anthropic",team="support"`

		It("doubles the support team's claude-sonnet-4 cost and tokens yesterday", func() {
			s := samples()
			for _, name := range []string{"llm_cost_usd_total", "llm_tokens_total"} {
				ratio := dayIncrease(s, name, sonnet, yesterday) / dayIncrease(s, name, sonnet, yesterday.AddDate(0, 0, -7))
				Expect(ratio).To(BeNumerically("~", 2.0, 0.15), name)
			}
		})
		It("leaves every other series alone", func() {
			s := samples()
			for _, sr := range seriesList {
				if sr.Team == anomalyTeam && sr.Model == anomalyModel {
					continue
				}
				labels := `model="` + sr.Model + `",provider="` + sr.Provider + `",team="` + sr.Team + `"`
				ratio := dayIncrease(s, "llm_cost_usd_total", labels, yesterday) / dayIncrease(s, "llm_cost_usd_total", labels, yesterday.AddDate(0, 0, -7))
				Expect(ratio).To(BeNumerically("~", 1.0, 0.15), labels)
			}
		})
	})
})
