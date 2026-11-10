// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Verdicts of one check.
const (
	Pass     = "pass"
	Fail     = "fail"
	KnownGap = "known-gap" // failed on a declared gap: "known gap: #N"
	NotRun   = "notrun"    // an earlier step failed, so the check never started
)

// Check is one labelled check of the drill and what it found.
type Check struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	// Issue is the issue that fixes a known gap, "#516".
	Issue   string `json:"issue,omitempty"`
	Message string `json:"message,omitempty"`
}

// Marks are the instants the RTO and RPO are computed from.
type Marks struct {
	DumpStart time.Time `json:"dumpStart"`
	DumpEnd   time.Time `json:"dumpEnd"`
	// Disaster is t_disaster: the moment the suite declares the source lost.
	Disaster        time.Time `json:"disaster"`
	TargetInstalled time.Time `json:"targetInstalled"`
	Restored        time.Time `json:"restored"`
	HeldChecksDone  time.Time `json:"heldChecksDone"`
	Released        time.Time `json:"released"`
	FirstRunDone    time.Time `json:"firstRunDone"`
	QueueDrained    time.Time `json:"queueDrained"`
	// AllChecksPassed is when the last released check passed.
	AllChecksPassed time.Time `json:"allChecksPassed"`
	// LatestMarker is the time of the newest RPO marker present in the target.
	LatestMarker time.Time `json:"latestMarker"`
}

// Timings are the sub-times and the two numbers a release gate records. Decision 15 of ADR 0085
// sets no target for either: nothing here fails a run.
type Timings struct {
	RTOSeconds float64            `json:"rtoSeconds"`
	RPOSeconds float64            `json:"rpoSeconds"`
	Phases     map[string]float64 `json:"phasesSeconds"`
}

// Compute derives the timings from the marks. A mark that is zero leaves its phase out (a run that
// stopped early still reports what it measured); RTO and RPO need their two ends.
func (m Marks) Compute() Timings {
	t := Timings{Phases: map[string]float64{}}
	add := func(name string, from, to time.Time) {
		if !from.IsZero() && !to.IsZero() {
			t.Phases[name] = to.Sub(from).Seconds()
		}
	}
	add("backup", m.DumpStart, m.DumpEnd)
	add("targetInstall", m.Disaster, m.TargetInstalled)
	add("restore", m.TargetInstalled, m.Restored)
	add("heldChecks", m.Restored, m.HeldChecksDone)
	add("release", m.HeldChecksDone, m.Released)
	add("firstRun", m.Released, m.FirstRunDone)
	add("queueDrained", m.Released, m.QueueDrained)
	if !m.Disaster.IsZero() && !m.AllChecksPassed.IsZero() {
		t.RTOSeconds = m.AllChecksPassed.Sub(m.Disaster).Seconds()
	}
	if !m.Disaster.IsZero() && !m.LatestMarker.IsZero() {
		t.RPOSeconds = m.Disaster.Sub(m.LatestMarker).Seconds()
	}
	return t
}

// Result is drill-result.json: what a release gate reads.
type Result struct {
	RunID    string    `json:"runId"`
	Commit   string    `json:"commit"`
	Chart    string    `json:"chart"`
	Topology *Topology `json:"topology"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Marks    Marks     `json:"marks"`
	Timings  Timings   `json:"timings"`
	Checks   []Check   `json:"checks"`
	// KnownGaps lists the checks that failed on a declared gap, with their issues.
	KnownGaps []Check `json:"knownGaps"`
	// Verdict is "pass" when every check passed or failed only on a known gap, else "fail".
	Verdict string `json:"verdict"`
}

// Finish fills Timings, KnownGaps and Verdict from Marks and Checks.
func (r *Result) Finish() {
	r.Timings = r.Marks.Compute()
	r.KnownGaps = nil
	r.Verdict = Pass
	sort.SliceStable(r.Checks, func(i, j int) bool { return r.Checks[i].Name < r.Checks[j].Name })
	for _, c := range r.Checks {
		switch c.Verdict {
		case KnownGap:
			r.KnownGaps = append(r.KnownGaps, c)
		case Pass:
		default:
			r.Verdict = Fail
		}
	}
}

// Write puts the result at path.
func (r *Result) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// GapPrefix starts the message of a known-gap failure; the issue follows.
const GapPrefix = "known gap: "

// GapMessage is the failure message of a spec that fails on a declared gap.
func GapMessage(issue, what string) string { return fmt.Sprintf("%s%s: %s", GapPrefix, issue, what) }
