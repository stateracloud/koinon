// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// shardEnv names the mode group one run covers (issue #1303). Empty or "all" runs every mode in one
// process, as `make test-chart-smoke` does without CHART_SMOKE_SHARD. CI runs one process per
// shard, so each shard has its own time budget.
const shardEnv = "KOINON_CHARTSMOKE_SHARD"

// shards groups the install modes (the charts/koinon/ci/<mode>-values.yaml names). The KMS
// modes, which start an extra container each, are together in one smaller group. Every mode
// is in exactly one group: TestShardsCoverEveryMode fails for a new values file that is in none, so
// a mode never drops out of CI without a message.
var shards = map[string][]string{
	"core": {
		"cnpg-kubernetes", "cnpg-standalone", "external-durable", "external-kubernetes",
		"external-standalone", "run-evidence",
	},
	"external": {
		"checkpoints", "checkpoints-readonly", "external-mcp-koinon", "external-operator-secrets",
		"external-restore-hold", "external-standalone-restore-hold", "openshell", "substrate", "substrate-egress",
	},
	"kms": {"kms-awskms", "kms-openbao", "kms-openbao-mcp-koinon"},
}

// shardNames is the sorted list of the shard names.
func shardNames() []string {
	names := make([]string, 0, len(shards))
	for n := range shards {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// modesOfShard returns the values files of `all` that belong to the shard, in the order of `all`.
// An empty shard or "all" returns every file. An unknown shard, or a values file that is in no
// shard, is an error, not an empty selection.
func modesOfShard(shard string, all []string) ([]string, error) {
	mode := func(vf string) string { return strings.TrimSuffix(filepath.Base(vf), "-values.yaml") }
	if shard == "" || shard == "all" {
		return all, nil
	}
	want, ok := shards[shard]
	if !ok {
		return nil, fmt.Errorf("%s=%q is not a shard: use one of %s, or all", shardEnv, shard, strings.Join(shardNames(), ", "))
	}
	var out []string
	for _, vf := range all {
		m := mode(vf)
		if slices.Contains(want, m) {
			out = append(out, vf)
		} else if shardOf(m) == "" {
			return nil, fmt.Errorf("mode %q is in no shard: add it to shards in test/chartsmoke/shard_test.go", m)
		}
	}
	return out, nil
}

// shardOf returns the shard that holds the mode, or "".
func shardOf(mode string) string {
	for _, n := range shardNames() {
		if slices.Contains(shards[n], mode) {
			return n
		}
	}
	return ""
}

// selectedValuesFiles lists the values files this run covers: all of them, or the shard in the
// environment.
func selectedValuesFiles() []string {
	files, err := modesOfShard(os.Getenv(shardEnv), valuesFiles())
	if err != nil {
		panic(err)
	}
	return files
}

// A plain test, not a spec: it needs neither Docker nor envtest.
func TestShardsCoverEveryMode(t *testing.T) {
	all := valuesFiles()
	seen := map[string]string{}
	for _, n := range shardNames() {
		for _, m := range shards[n] {
			if prev, dup := seen[m]; dup {
				t.Errorf("mode %q is in shards %q and %q", m, prev, n)
			}
			seen[m] = n
		}
	}
	for _, vf := range all {
		m := strings.TrimSuffix(filepath.Base(vf), "-values.yaml")
		if _, ok := seen[m]; !ok {
			t.Errorf("mode %q has a values file but is in no shard", m)
		}
		delete(seen, m)
	}
	for m, n := range seen {
		t.Errorf("shard %q lists mode %q, which has no values file in %s/ci", n, m, chartDir)
	}
	// The union of the shards is every file, once.
	var got []string
	for _, n := range shardNames() {
		files, err := modesOfShard(n, all)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, files...)
	}
	if len(got) != len(all) {
		t.Errorf("the shards hold %d modes, there are %d values files", len(got), len(all))
	}
}

func TestModesOfShard(t *testing.T) {
	all := []string{"d/core-a-values.yaml", "d/kms-b-values.yaml"}
	if got, err := modesOfShard("", all); err != nil || !slices.Equal(got, all) {
		t.Errorf("empty shard: got %v, %v; want every file", got, err)
	}
	if got, err := modesOfShard("all", all); err != nil || !slices.Equal(got, all) {
		t.Errorf("all: got %v, %v; want every file", got, err)
	}
	if _, err := modesOfShard("nope", all); err == nil || !strings.Contains(err.Error(), "core") {
		t.Errorf("unknown shard: got %v; want an error that lists the shards", err)
	}
	// A file in no shard is an error in every named shard, so it cannot drop out of CI.
	if _, err := modesOfShard("kms", append([]string{"d/brand-new-values.yaml"}, all...)); err == nil || !strings.Contains(err.Error(), "brand-new") {
		t.Errorf("a mode in no shard: got %v; want an error that names it", err)
	}
	got, err := modesOfShard("kms", []string{"d/kms-openbao-values.yaml", "d/checkpoints-values.yaml"})
	if err != nil || !slices.Equal(got, []string{"d/kms-openbao-values.yaml"}) {
		t.Errorf("kms: got %v, %v", got, err)
	}
}
