// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The fleet breaker of the front check (issue #1481): silence on more than one worker, or on a large
// share of the fleet, gives no verdict; one dead front, or a few on one worker, still does.
func TestFleetBreaker(t *testing.T) {
	const window = time.Minute
	now := time.Unix(1_800_000_000, 0)
	id := func(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{n}, Valid: true} }
	type mark struct {
		down   bool
		worker string
		age    time.Duration
	}
	for _, tc := range []struct {
		name  string
		marks []mark
		open  bool
	}{
		{"one dead front alone", []mark{{down: true, worker: "a"}}, false},
		{"one dead front among live ones", []mark{{down: true, worker: "a"}, {worker: "a"}, {worker: "b"}}, false},
		{"two dead fronts on one worker", []mark{{down: true, worker: "a"}, {down: true, worker: "a"}}, false},
		{"silence on two workers", []mark{{down: true, worker: "a"}, {down: true, worker: "b"}, {worker: "c"}}, true},
		{"three silent of four on one worker", []mark{{down: true, worker: "a"}, {down: true, worker: "a"}, {down: true, worker: "a"}, {worker: "a"}}, true},
		{"three silent of ten on one worker", append(
			[]mark{{down: true, worker: "a"}, {down: true, worker: "a"}, {down: true, worker: "a"}},
			slices.Repeat([]mark{{worker: "a"}}, 7)...), false},
		{"an unknown worker counts as its own", []mark{{down: true}, {down: true}}, true},
		{"a mark older than the window does not count", []mark{{down: true, worker: "a"}, {down: true, worker: "b", age: window}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr crashTracker
			for i, m := range tc.marks {
				tr.noteFront(id(byte(i+1)), now.Add(-m.age), m.down, m.worker)
			}
			st, changed := tr.fleet(now, window)
			if st.open != tc.open {
				t.Fatalf("open = %v, want %v (%+v)", st.open, tc.open, st)
			}
			if changed != tc.open {
				t.Fatalf("changed = %v on the first evaluation, want %v", changed, tc.open)
			}
			if _, again := tr.fleet(now, window); again {
				t.Fatal("a second evaluation with the same marks must not report a change")
			}
		})
	}
}
