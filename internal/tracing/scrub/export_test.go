// SPDX-License-Identifier: Apache-2.0

package scrub

// GitleaksSkipped lists the gitleaks rules that did not compile; a test requires none.
func GitleaksSkipped() []string { return gitleaks().skipped }

// GitleaksExcluded is the set of gitleaks rules the scrubber does not apply.
func GitleaksExcluded() map[string]string { return gitleaksExcluded }

// GitleaksMatches returns the IDs of the gitleaks rules that match in alone (the rule's keywords
// must occur, as in gitleaks), and the IDs of rules whose expression matches although the keyword
// pre-filter skipped them. A rule with a loose expression and a keyword (sourcegraph's 40 hex
// digits) is in the second list for text without the keyword; that is gitleaks' own behaviour.
func GitleaksMatches(in string) (ids, skipped []string) {
	g := gitleaks()
	var small [8]uint64
	cand := g.kw.scan(in, small[:0])
	for i := range g.rules {
		if len(g.rules[i].find(g, in, nil)) == 0 {
			continue
		}
		if cand[i/64]&(1<<(i%64)) == 0 {
			skipped = append(skipped, g.rules[i].id)
		} else {
			ids = append(ids, g.rules[i].id)
		}
	}
	return ids, skipped
}

// GitleaksPrefilterSkips reports whether rule id matches in on its own although the keyword
// pre-filter would skip it.
func GitleaksPrefilterSkips(id, in string) bool {
	g := gitleaks()
	var small [8]uint64
	cand := g.kw.scan(in, small[:0])
	for i := range g.rules {
		if g.rules[i].id == id {
			return cand[i/64]&(1<<(i%64)) == 0 && len(g.rules[i].find(g, in, nil)) > 0
		}
	}
	return false
}

// Counting returns a copy of s that adds the length of every text its detectors read to *n, so a
// test can bound the work a scrub does without timing it.
func Counting(s *Scrubber, n *int) *Scrubber {
	c := *s
	c.work = func(l int) { *n += l }
	return &c
}

// GitleaksNoFloor is the set of gitleaks rules the scrubber applies without their entropy floor.
func GitleaksNoFloor() map[string]struct{} { return gitleaksNoFloor }
