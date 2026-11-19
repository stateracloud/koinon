// SPDX-License-Identifier: Apache-2.0

package upgradecheck_test

import (
	"testing"

	"github.com/stateracloud/koinon/test/e2e/upgradecheck"
)

const before = `
# a comment
install_id=11111111-1111-1111-1111-111111111111
migration=70
count.runs=3
ids.agents=a1,a2
ids.sandboxes=
kek=kek-1 active sha256:aa
`

func mustParse(t *testing.T, s string) upgradecheck.Snapshot {
	t.Helper()
	snap, err := upgradecheck.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name  string
		after string
		want  int
	}{
		{"same", before, 0},
		{"new migration, more runs, extra agent", `
install_id=11111111-1111-1111-1111-111111111111
migration=72
count.runs=4
ids.agents=a3,a2,a1
ids.sandboxes=s9
kek=kek-1 active sha256:aa`, 0},
		{"another install", `
install_id=22222222-2222-2222-2222-222222222222
migration=70
count.runs=3
ids.agents=a1,a2
ids.sandboxes=
kek=kek-1 active sha256:aa`, 1},
		{"migration went down", "install_id=11111111-1111-1111-1111-111111111111\nmigration=69\ncount.runs=3\nids.agents=a1,a2\nids.sandboxes=\nkek=kek-1 active sha256:aa", 1},
		{"run lost", "install_id=11111111-1111-1111-1111-111111111111\nmigration=70\ncount.runs=2\nids.agents=a1,a2\nids.sandboxes=\nkek=kek-1 active sha256:aa", 1},
		{"agent replaced", "install_id=11111111-1111-1111-1111-111111111111\nmigration=70\ncount.runs=3\nids.agents=a1,a9\nids.sandboxes=\nkek=kek-1 active sha256:aa", 1},
		{"key changed", "install_id=11111111-1111-1111-1111-111111111111\nmigration=70\ncount.runs=3\nids.agents=a1,a2\nids.sandboxes=\nkek=kek-2 active sha256:bb", 1},
		{"fact missing", "install_id=11111111-1111-1111-1111-111111111111\nmigration=70", 4},
	}
	b := mustParse(t, before)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := upgradecheck.Compare(b, mustParse(t, tc.after))
			if len(got) != tc.want {
				t.Fatalf("want %d findings, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}

func TestParseRejectsNoise(t *testing.T) {
	if _, err := upgradecheck.Parse("install_id=1\nERROR: no pod\n"); err == nil {
		t.Fatal("a line that is not key=value must be an error")
	}
}

func TestEmpty(t *testing.T) {
	s := mustParse(t, "count.runs=3\ncount.tasks=0")
	got := upgradecheck.Empty(s, "runs", "tasks", "agents")
	if len(got) != 2 || got[0] != "tasks" || got[1] != "agents" {
		t.Fatalf("got %v", got)
	}
}
