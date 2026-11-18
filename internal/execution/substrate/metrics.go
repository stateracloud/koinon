// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// DefaultMetricLabels is the label allowlist of the Substrate scrape (ADR 0023 label rule, amended
// by ADR 0119). Label values must come from small fixed sets: never an org, agent, user, ticket or
// run ID, and never an actor, atespace, tag, worker pod or node name, which grow with the data and
// leak tenant names to whoever scrapes. A series loses every label that is not listed, and series
// that become equal are summed. The list is what Substrate's own exporters label by in a set size:
// histogram buckets and quantiles, outcomes and codes, the actor state and the snapshot fidelity.
var DefaultMetricLabels = []string{"le", "quantile", "state", "outcome", "result", "code", "method", "fidelity", "sandbox_class", "reason"}

// maxScrapeBytes bounds one scrape.
const maxScrapeBytes = 8 << 20

// Scraper reads one Substrate metrics endpoint (ateapi, atelet or the router) and keeps only what
// the label allowlist permits.
type Scraper struct {
	// URL is the endpoint, for example http://api.ate-system.svc:9090/metrics.
	URL string
	// Client is the HTTP client. Nil uses one with a 10 s timeout.
	Client *http.Client
	// Labels is the allowlist. Nil uses [DefaultMetricLabels].
	Labels []string
}

// Scrape fetches the endpoint and returns the metric families with only allowed labels.
func (s *Scraper) Scrape(ctx context.Context) ([]*dto.MetricFamily, error) {
	fams, _, err := s.scrape(ctx)
	return fams, err
}

// scrape is Scrape that also returns how many series the merge had to drop.
func (s *Scraper) scrape(ctx context.Context) ([]*dto.MetricFamily, int, error) {
	cl := s.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("substrate: scrape %s: %w", s.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("substrate: scrape %s: %s", s.URL, resp.Status)
	}
	labels := s.Labels
	if labels == nil {
		labels = DefaultMetricLabels
	}
	return parse(io.LimitReader(resp.Body, maxScrapeBytes), labels)
}

// Parse reads Prometheus text and applies the allowlist.
func Parse(r io.Reader, allowed []string) ([]*dto.MetricFamily, error) {
	fams, _, err := parse(r, allowed)
	return fams, err
}

func parse(r io.Reader, allowed []string) ([]*dto.MetricFamily, int, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	byName, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, 0, fmt.Errorf("substrate: parse metrics: %w", err)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*dto.MetricFamily
	dropped := 0
	for _, n := range names {
		f, d := sanitize(byName[n], allowed)
		out = append(out, f)
		dropped += d
	}
	return out, dropped, nil
}

// Sanitize drops every label of a family that is not in allowed and merges the series that are then
// equal: counters, gauges and untyped values add up, histograms add bucket by bucket (a series with
// other bounds than the first is left out), summaries keep their count and sum and lose quantiles
// that cannot be added. [Collector] exposes a summary as count and sum, like Sanitize keeps it.
func Sanitize(f *dto.MetricFamily, allowed []string) *dto.MetricFamily {
	out, _ := sanitize(f, allowed)
	return out
}

// sanitize is Sanitize that also counts the series it had to leave out of a merge.
func sanitize(f *dto.MetricFamily, allowed []string) (_ *dto.MetricFamily, dropped int) {
	out := &dto.MetricFamily{Name: f.Name, Help: f.Help, Type: f.Type}
	merged := map[string]*dto.Metric{}
	var order []string
	for _, m := range f.GetMetric() {
		c := &dto.Metric{}
		for _, l := range m.GetLabel() {
			if slices.Contains(allowed, l.GetName()) {
				c.Label = append(c.Label, &dto.LabelPair{Name: l.Name, Value: l.Value})
			}
		}
		sort.Slice(c.Label, func(i, j int) bool { return c.Label[i].GetName() < c.Label[j].GetName() })
		var sb strings.Builder
		for _, l := range c.Label {
			fmt.Fprintf(&sb, "%s=%q,", l.GetName(), l.GetValue())
		}
		id := sb.String()
		have, ok := merged[id]
		if !ok {
			c.Counter, c.Gauge, c.Untyped = m.Counter, m.Gauge, m.Untyped
			c.Histogram, c.Summary = m.Histogram, m.Summary
			if c.Counter != nil {
				c.Counter = &dto.Counter{Value: m.Counter.Value}
			}
			if c.Gauge != nil {
				c.Gauge = &dto.Gauge{Value: m.Gauge.Value}
			}
			if c.Untyped != nil {
				c.Untyped = &dto.Untyped{Value: m.Untyped.Value}
			}
			if h := m.Histogram; h != nil {
				c.Histogram = &dto.Histogram{SampleCount: h.SampleCount, SampleSum: h.SampleSum}
				for _, b := range h.Bucket {
					c.Histogram.Bucket = append(c.Histogram.Bucket, &dto.Bucket{UpperBound: b.UpperBound, CumulativeCount: b.CumulativeCount})
				}
			}
			if s := m.Summary; s != nil {
				c.Summary = &dto.Summary{SampleCount: s.SampleCount, SampleSum: s.SampleSum}
			}
			merged[id] = c
			order = append(order, id)
			continue
		}
		if !addMetric(have, m) {
			dropped++
		}
	}
	for _, id := range order {
		out.Metric = append(out.Metric, merged[id])
	}
	return out, dropped
}

// addMetric adds src to dst. It reports false when src cannot be added, because it is of another
// kind or a histogram with other bucket bounds.
func addMetric(dst, src *dto.Metric) bool {
	switch {
	case dst.Counter != nil && src.Counter != nil:
		*dst.Counter.Value += src.Counter.GetValue()
	case dst.Gauge != nil && src.Gauge != nil:
		*dst.Gauge.Value += src.Gauge.GetValue()
	case dst.Untyped != nil && src.Untyped != nil:
		*dst.Untyped.Value += src.Untyped.GetValue()
	case dst.Histogram != nil && src.Histogram != nil:
		h, s := dst.Histogram, src.Histogram
		if len(h.Bucket) != len(s.Bucket) {
			return false
		}
		for i := range h.Bucket {
			if h.Bucket[i].GetUpperBound() != s.Bucket[i].GetUpperBound() {
				return false
			}
		}
		for i := range h.Bucket {
			*h.Bucket[i].CumulativeCount += s.Bucket[i].GetCumulativeCount()
		}
		*h.SampleCount += s.GetSampleCount()
		*h.SampleSum += s.GetSampleSum()
	case dst.Summary != nil && src.Summary != nil:
		*dst.Summary.SampleCount += src.Summary.GetSampleCount()
		*dst.Summary.SampleSum += src.Summary.GetSampleSum()
	default:
		return false
	}
	return true
}

// Collector exposes a Scraper's metrics through a Prometheus registry. It scrapes when it is
// collected; a failed scrape yields no series and one error metric, so a dead Substrate shows as a
// gap and as koinon_substrate_scrape_error, not as a failed /metrics.
type Collector struct {
	Scraper *Scraper
	// Timeout bounds one scrape. Default 5 s.
	Timeout time.Duration
	// Logger gets the one line that says series were dropped. Nil uses slog.Default().
	Logger *slog.Logger

	dropped atomic.Uint64
	logOnce sync.Once
}

var (
	scrapeErr     = prometheus.NewDesc("koinon_substrate_scrape_error", "1 when the last scrape of a Substrate metrics endpoint failed.", nil, nil)
	scrapeDropped = prometheus.NewDesc("koinon_substrate_scrape_dropped_series_total", "Series left out when dropping labels merged series that could not be added (histograms with other bucket bounds). No labels: ADR 0023.", nil, nil)
)

// Describe implements prometheus.Collector. It sends nothing: the series are not known in advance
// (an unchecked collector).
func (c *Collector) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	t := c.Timeout
	if t <= 0 {
		t = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	fams, dropped, err := c.Scraper.scrape(ctx)
	v := 0.0
	if err != nil {
		v = 1
	}
	ch <- prometheus.MustNewConstMetric(scrapeErr, prometheus.GaugeValue, v)
	if dropped > 0 {
		c.dropped.Add(uint64(dropped))
		c.logOnce.Do(func() {
			l := c.Logger
			if l == nil {
				l = slog.Default()
			}
			l.Warn("substrate metrics: series dropped while merging after label removal; see koinon_substrate_scrape_dropped_series_total", "url", c.Scraper.URL)
		})
	}
	ch <- prometheus.MustNewConstMetric(scrapeDropped, prometheus.CounterValue, float64(c.dropped.Load()))
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			names := make([]string, len(m.GetLabel()))
			vals := make([]string, len(m.GetLabel()))
			for i, l := range m.GetLabel() {
				names[i], vals[i] = l.GetName(), l.GetValue()
			}
			desc := prometheus.NewDesc(f.GetName(), f.GetHelp(), names, nil)
			var pm prometheus.Metric
			var merr error
			switch {
			case m.Counter != nil:
				pm, merr = prometheus.NewConstMetric(desc, prometheus.CounterValue, m.Counter.GetValue(), vals...)
			case m.Gauge != nil:
				pm, merr = prometheus.NewConstMetric(desc, prometheus.GaugeValue, m.Gauge.GetValue(), vals...)
			case m.Untyped != nil:
				pm, merr = prometheus.NewConstMetric(desc, prometheus.UntypedValue, m.Untyped.GetValue(), vals...)
			case m.Histogram != nil:
				buckets := map[float64]uint64{}
				for _, b := range m.Histogram.Bucket {
					buckets[b.GetUpperBound()] = b.GetCumulativeCount()
				}
				pm, merr = prometheus.NewConstHistogram(desc, m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum(), buckets, vals...)
			case m.Summary != nil: // count and sum only: quantiles of merged series cannot be added
				pm, merr = prometheus.NewConstSummary(desc, m.Summary.GetSampleCount(), m.Summary.GetSampleSum(), map[float64]float64{}, vals...)
			default:
				continue
			}
			if merr == nil {
				ch <- pm
			}
		}
	}
}
