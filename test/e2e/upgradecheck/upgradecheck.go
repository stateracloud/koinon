// SPDX-License-Identifier: Apache-2.0

// Package upgradecheck compares two snapshots of an install: one taken on the previous release (N-1) after
// the seed, one taken on the build under test (N) after `helm upgrade` (issue 519). A snapshot is text, one fact
// per line, so the e2e shell script and the Ginkgo spec share it and this package tests the rules offline.
//
// Line forms:
//
//	install_id=<uuid>        must be equal
//	migration=<n>            must not go down (the new build may add migrations)
//	count.<name>=<n>         must not go down (the seed is kept; the upgrade may add rows)
//	ids.<name>=<a>,<b>,...   every id of the first snapshot must be in the second (nothing is lost or replaced)
//	kek=<id> <state> <fp>    must be equal (the upgrade changes no key)
//
// Any other key must be equal. Lines that start with # and empty lines are ignored.
package upgradecheck

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Snapshot maps a key to its values. A key can occur more than once (the kek lines).
type Snapshot map[string][]string

// Parse reads the text form. A line without "=" is an error: it is a sign that the collector printed an error.
func Parse(text string) (Snapshot, error) {
	s := Snapshot{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: %q is not key=value", i+1, line)
		}
		s[k] = append(s[k], v)
	}
	return s, nil
}

// Compare returns one message per rule that the after-snapshot breaks. An empty result means the install converged.
func Compare(before, after Snapshot) []string {
	var out []string
	keys := make([]string, 0, len(before))
	for k := range before {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b, a := before[k], after[k]
		if len(a) == 0 {
			out = append(out, fmt.Sprintf("%s: present before the upgrade, missing after", k))
			continue
		}
		switch {
		case k == "migration" || strings.HasPrefix(k, "count."):
			bn, berr := strconv.Atoi(b[0])
			an, aerr := strconv.Atoi(a[0])
			if berr != nil || aerr != nil {
				out = append(out, fmt.Sprintf("%s: not a number (before %q, after %q)", k, b[0], a[0]))
			} else if an < bn {
				out = append(out, fmt.Sprintf("%s: went down from %d to %d", k, bn, an))
			}
		case strings.HasPrefix(k, "ids."):
			have := map[string]bool{}
			for _, id := range splitIDs(a[0]) {
				have[id] = true
			}
			for _, id := range splitIDs(b[0]) {
				if !have[id] {
					out = append(out, fmt.Sprintf("%s: %s was there before the upgrade and is gone after", k, id))
				}
			}
		default:
			if !sameSet(b, a) {
				out = append(out, fmt.Sprintf("%s: changed from %q to %q", k, b, a))
			}
		}
	}
	return out
}

func splitIDs(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func sameSet(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\n") == strings.Join(y, "\n")
}

// Empty lists the counts that are zero in the snapshot. The seed must leave rows in each table the
// upgrade claims to keep: a test that compares zero with zero proves nothing.
func Empty(s Snapshot, names ...string) []string {
	var out []string
	for _, n := range names {
		v := s["count."+n]
		if len(v) == 0 || v[0] == "0" {
			out = append(out, n)
		}
	}
	return out
}
