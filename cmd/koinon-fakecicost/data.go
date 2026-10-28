// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"time"
)

const (
	// historyDays is how many days of data exist, counting back from today.
	historyDays = 60
	// dateLayout is the date format of the API.
	dateLayout = "2006-01-02"

	// anomalyRepo is the repository whose CI cost doubles yesterday, because a new matrix job fans
	// the "test" workflow out. The RT1 write-up checks the agent names it.
	anomalyRepo     = "checkout-web"
	anomalyWorkflow = "test"
)

var (
	repos     = []string{"checkout-web", "payments-api", "data-pipeline", "infra"}
	workflows = []string{"build", "test", "deploy", "e2e"}
	runners   = []string{"linux-small", "linux-large", "macos", "gpu"}

	// perMinute is the runner types' price in USD per minute.
	perMinute = map[string]float64{"linux-small": 0.008, "linux-large": 0.032, "macos": 0.08, "gpu": 0.16}
)

// row is one day's usage of one repository, workflow and runner type.
type row struct {
	Date     string
	Repo     string
	Workflow string
	Runner   string
	Runs     int
	Minutes  float64
	CostUSD  float64
}

// unit returns a deterministic number in [0, 1) for the given parts.
func unit(parts ...string) float64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return float64(h.Sum64()>>11) / float64(uint64(1)<<53)
}

// combos says which (repo, workflow, runner) combinations exist, and their typical runs per
// weekday. Not every combination does: only a few are used, as in a real organisation.
func combos(repo, workflow, runner string) (runs float64, ok bool) {
	u := unit("combo", repo, workflow, runner)
	switch runner {
	case "gpu":
		return 3, repo == "data-pipeline" && workflow == "test"
	case "macos":
		return 6, repo == "checkout-web" && (workflow == "build" || workflow == "e2e")
	case "linux-large":
		return 12, workflow == "build" || (workflow == "e2e" && u > 0.3)
	default:
		return 20, workflow != "e2e" || u > 0.5
	}
}

// dayRows returns the usage rows of one day, sorted. Today is the clock's date; yesterday carries
// the planted anomaly. The result depends only on the day and on which day is yesterday.
func dayRows(day, yesterday time.Time) []row {
	date := day.Format(dateLayout)
	weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
	var out []row
	for _, repo := range repos {
		for _, workflow := range workflows {
			for _, runner := range runners {
				base, ok := combos(repo, workflow, runner)
				if !ok {
					continue
				}
				if weekend {
					base *= 0.25
				}
				runs := math.Round(base * (0.9 + 0.2*unit("runs", date, repo, workflow, runner)))
				minutes := runs * (4 + 8*unit("len", repo, workflow, runner)) * (0.95 + 0.1*unit("dur", date, repo, workflow, runner))
				out = append(out, row{Date: date, Repo: repo, Workflow: workflow, Runner: runner, Runs: int(runs), Minutes: minutes})
			}
		}
	}
	if day.Format(dateLayout) == yesterday.Format(dateLayout) {
		plantAnomaly(out)
	}
	for i := range out {
		out[i].Minutes = round2(out[i].Minutes)
		out[i].CostUSD = round2(out[i].Minutes * perMinute[out[i].Runner])
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Repo+"/"+a.Workflow+"/"+a.Runner < b.Repo+"/"+b.Workflow+"/"+b.Runner
	})
	return out
}

// plantAnomaly scales the anomaly repository's test workflow so the repository's total cost doubles:
// a new matrix job fans the workflow out, which multiplies its runs and minutes.
func plantAnomaly(rows []row) {
	var total, test float64
	for _, r := range rows {
		if r.Repo != anomalyRepo {
			continue
		}
		c := r.Minutes * perMinute[r.Runner]
		total += c
		if r.Workflow == anomalyWorkflow {
			test += c
		}
	}
	if test == 0 {
		return
	}
	factor := 1 + total/test
	for i := range rows {
		if rows[i].Repo == anomalyRepo && rows[i].Workflow == anomalyWorkflow {
			rows[i].Runs = int(math.Round(float64(rows[i].Runs) * factor))
			rows[i].Minutes *= factor
		}
	}
}

// parseDate parses a YYYY-MM-DD date in UTC.
func parseDate(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("%s is required (YYYY-MM-DD)", name)
	}
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not a YYYY-MM-DD date", name, s)
	}
	return t, nil
}
