// SPDX-License-Identifier: Apache-2.0

package e2e_test

// Helpers of the agentd failover spec (issue 9, AG1): the spec itself is in quickstart_test.go
// because it needs the org, the connection and the agent the quickstart steps made.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	. "github.com/onsi/gomega"

	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
)

// eventLog drains a WatchRun stream in the background and keeps every event it delivered, in the
// order it arrived, so a spec can check that no seq came twice.
type eventLog struct {
	mu     sync.Mutex
	events []*runv1alpha1.RunEvent
}

func collectEvents(w *watcher) *eventLog {
	l := &eventLog{}
	go func() {
		for msg := range w.items {
			if ev := msg.GetEvent(); ev != nil {
				l.mu.Lock()
				l.events = append(l.events, ev)
				l.mu.Unlock()
			}
		}
	}()
	return l
}

// seqs returns the seq of every event delivered so far, in arrival order.
func (l *eventLog) seqs() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int64, 0, len(l.events))
	for _, ev := range l.events {
		out = append(out, ev.GetSeq())
	}
	return out
}

// psqlPrimary runs one query on the primary Postgres pod as its local superuser and returns the
// rows, one string per line with the columns separated by "|".
func psqlPrimary(ctx context.Context, g Gomega, kubectl, query string) []string {
	pod, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "pod", "-l", "cnpg.io/instanceRole=primary", "-o", "name").Output()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.TrimSpace(string(pod))).NotTo(BeEmpty(), "the primary Postgres pod")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "exec", strings.TrimSpace(string(pod)), "-c", "postgres", "--",
		"psql", "-U", "postgres", "-d", "koinon", "-At", "-F", "|", "-c", query).CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), string(out))
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// runOwner is the owner columns of a run row.
type runOwner struct {
	State     string
	Owner     string
	Epoch     int64
	Takeovers int64
}

// readRunOwner reads who owns the run and under which epoch.
func readRunOwner(ctx context.Context, g Gomega, kubectl, run string) runOwner {
	rows := psqlPrimary(ctx, g, kubectl,
		"SELECT state, coalesce(owner_id, ''), epoch, takeovers FROM runs WHERE id = '"+run+"'")
	g.Expect(rows).To(HaveLen(1), "the run row")
	f := strings.Split(rows[0], "|")
	g.Expect(f).To(HaveLen(4), rows[0])
	epoch, err := strconv.ParseInt(f[2], 10, 64)
	g.Expect(err).NotTo(HaveOccurred())
	takeovers, err := strconv.ParseInt(f[3], 10, 64)
	g.Expect(err).NotTo(HaveOccurred())
	return runOwner{State: f[0], Owner: f[1], Epoch: epoch, Takeovers: takeovers}
}

// storedEvent is one run_events row, as far as the failover spec checks it.
type storedEvent struct {
	Seq, Epoch int64
	Kind       string
	// SourceSeq is the writer's own sequence number, 0 when the event has none.
	SourceSeq int64
}

// readRunEvents reads the run's events in seq order.
func readRunEvents(ctx context.Context, g Gomega, kubectl, run string) []storedEvent {
	rows := psqlPrimary(ctx, g, kubectl,
		"SELECT seq, epoch, kind, coalesce(source_seq, 0) FROM run_events WHERE run_id = '"+run+"' ORDER BY seq")
	out := make([]storedEvent, 0, len(rows))
	for _, line := range rows {
		f := strings.Split(line, "|")
		g.Expect(f).To(HaveLen(4), line)
		var e storedEvent
		var err error
		e.Seq, err = strconv.ParseInt(f[0], 10, 64)
		g.Expect(err).NotTo(HaveOccurred(), line)
		e.Epoch, err = strconv.ParseInt(f[1], 10, 64)
		g.Expect(err).NotTo(HaveOccurred(), line)
		e.Kind = f[2]
		e.SourceSeq, err = strconv.ParseInt(f[3], 10, 64)
		g.Expect(err).NotTo(HaveOccurred(), line)
		out = append(out, e)
	}
	return out
}

// checkNoDuplicateEvents is the failover invariant on the stored events: seq runs 1..n without a
// gap or a repeat, the epoch never goes back (a fenced-out owner writes nothing after the takeover),
// and no writer sequence number is recorded twice within an epoch.
func checkNoDuplicateEvents(g Gomega, events []storedEvent) {
	type key struct{ epoch, source int64 }
	seen := map[key]int64{}
	var prevEpoch int64
	for i, e := range events {
		g.Expect(e.Seq).To(Equal(int64(i+1)), "seq is gap-free and unique: %+v", events)
		g.Expect(e.Epoch).To(BeNumerically(">=", prevEpoch), "the epoch never goes back at seq %d", e.Seq)
		prevEpoch = e.Epoch
		if e.SourceSeq == 0 {
			continue
		}
		k := key{e.Epoch, e.SourceSeq}
		prior, dup := seen[k]
		g.Expect(dup).To(BeFalse(), "%s", fmt.Sprintf("source_seq %d of epoch %d is recorded at seq %d and %d", e.SourceSeq, e.Epoch, prior, e.Seq))
		seen[k] = e.Seq
	}
}
