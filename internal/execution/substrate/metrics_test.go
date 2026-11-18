// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/execution/substrate"
)

const scrapeBody = `# HELP ate_actor_resumes_total Resumes.
# TYPE ate_actor_resumes_total counter
ate_actor_resumes_total{atespace="org-acme",actor="agent-1",outcome="ok"} 3
ate_actor_resumes_total{atespace="org-bob",actor="agent-9",outcome="ok"} 4
ate_actor_resumes_total{atespace="org-bob",actor="agent-9",outcome="error"} 1
# HELP ate_actors Actors by state.
# TYPE ate_actors gauge
ate_actors{state="running",worker_pod="w-1"} 2
ate_actors{state="running",worker_pod="w-2"} 5
# HELP ate_resume_seconds Resume time.
# TYPE ate_resume_seconds histogram
ate_resume_seconds_bucket{actor="a",le="0.5"} 1
ate_resume_seconds_bucket{actor="a",le="1"} 2
ate_resume_seconds_bucket{actor="a",le="+Inf"} 2
ate_resume_seconds_sum{actor="a"} 0.9
ate_resume_seconds_count{actor="a"} 2
ate_resume_seconds_bucket{actor="b",le="0.5"} 0
ate_resume_seconds_bucket{actor="b",le="1"} 1
ate_resume_seconds_bucket{actor="b",le="+Inf"} 1
ate_resume_seconds_sum{actor="b"} 0.7
ate_resume_seconds_count{actor="b"} 1
`

var _ = Describe("the metrics scrape", func() {
	It("drops every label that is not on the allowlist and sums the series that become equal", func() {
		fams, err := substrate.Parse(strings.NewReader(scrapeBody), substrate.DefaultMetricLabels)
		Expect(err).NotTo(HaveOccurred())
		series := map[string]float64{}
		for _, f := range fams {
			for _, m := range f.GetMetric() {
				id := f.GetName()
				for _, l := range m.GetLabel() {
					Expect(substrate.DefaultMetricLabels).To(ContainElement(l.GetName()), "label %s must not survive", l.GetName())
					id += "|" + l.GetName() + "=" + l.GetValue()
				}
				switch {
				case m.Counter != nil:
					series[id] = m.Counter.GetValue()
				case m.Gauge != nil:
					series[id] = m.Gauge.GetValue()
				case m.Histogram != nil:
					series[id+"|count"] = float64(m.Histogram.GetSampleCount())
					series[id+"|sum"] = m.Histogram.GetSampleSum()
				}
			}
		}
		Expect(series).To(HaveKeyWithValue("ate_actor_resumes_total|outcome=ok", 7.0), "two orgs' series add up, the org and actor names are gone")
		Expect(series).To(HaveKeyWithValue("ate_actor_resumes_total|outcome=error", 1.0))
		Expect(series).To(HaveKeyWithValue("ate_actors|state=running", 7.0))
		Expect(series).To(HaveKeyWithValue("ate_resume_seconds|count", 3.0))
		Expect(series["ate_resume_seconds|sum"]).To(BeNumerically("~", 1.6, 1e-9))
	})

	It("counts the series a merge had to drop, with no label, and keeps summaries as count and sum", func() {
		body := `# TYPE h histogram
h_bucket{actor="a",le="1"} 1
h_bucket{actor="a",le="+Inf"} 1
h_sum{actor="a"} 1
h_count{actor="a"} 1
h_bucket{actor="b",le="2"} 1
h_bucket{actor="b",le="+Inf"} 1
h_sum{actor="b"} 2
h_count{actor="b"} 1
# TYPE s summary
s{actor="a",quantile="0.5"} 1
s_sum{actor="a"} 3
s_count{actor="a"} 2
s{actor="b",quantile="0.5"} 1
s_sum{actor="b"} 4
s_count{actor="b"} 3
`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		DeferCleanup(srv.Close)
		var logs strings.Builder
		reg := prometheus.NewPedanticRegistry()
		Expect(reg.Register(&substrate.Collector{
			Scraper: &substrate.Scraper{URL: srv.URL},
			Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		})).To(Succeed())
		Expect(testutil.GatherAndCompare(reg, strings.NewReader(`# HELP koinon_substrate_scrape_dropped_series_total Series left out when dropping labels merged series that could not be added (histograms with other bucket bounds). No labels: ADR 0023.
# TYPE koinon_substrate_scrape_dropped_series_total counter
koinon_substrate_scrape_dropped_series_total 1
`), "koinon_substrate_scrape_dropped_series_total")).To(Succeed())
		Expect(testutil.GatherAndCount(reg, "s")).To(Equal(1), "the two summaries merge into one")
		Expect(testutil.GatherAndCount(reg, "h")).To(Equal(1))
		Expect(strings.Count(logs.String(), "series dropped")).To(Equal(1), "logged once across scrapes: %s", logs.String())
	})

	It("scrapes over HTTP and exposes the result through a registry, with an error gauge", func() {
		fail := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if fail {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(scrapeBody))
		}))
		DeferCleanup(srv.Close)
		reg := prometheus.NewPedanticRegistry()
		Expect(reg.Register(&substrate.Collector{Scraper: &substrate.Scraper{URL: srv.URL}})).To(Succeed())

		Expect(testutil.GatherAndCount(reg, "ate_actors", "ate_actor_resumes_total")).To(Equal(3))
		Expect(testutil.GatherAndCompare(reg, strings.NewReader(`# HELP koinon_substrate_scrape_error 1 when the last scrape of a Substrate metrics endpoint failed.
# TYPE koinon_substrate_scrape_error gauge
koinon_substrate_scrape_error 0
`), "koinon_substrate_scrape_error")).To(Succeed())

		fail = true
		Expect(testutil.GatherAndCount(reg, "ate_actors")).To(BeZero())
		Expect(testutil.GatherAndCompare(reg, strings.NewReader(`# HELP koinon_substrate_scrape_error 1 when the last scrape of a Substrate metrics endpoint failed.
# TYPE koinon_substrate_scrape_error gauge
koinon_substrate_scrape_error 1
`), "koinon_substrate_scrape_error")).To(Succeed())
	})
})
