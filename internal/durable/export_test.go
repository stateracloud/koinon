// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// VersionLabel and NewCollectorFor let the metric specs run without a database.
var VersionLabel = versionLabel

func NewCollectorFor(src func(context.Context) (map[string]float64, map[string]float64, error), current string, ttl time.Duration) prometheus.Collector {
	return newCollector(func(ctx context.Context) (counts, error) {
		p, e, err := src(ctx)
		return counts{pending: p, executors: e}, err
	}, current, ttl)
}
