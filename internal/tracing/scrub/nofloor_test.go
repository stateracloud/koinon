// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"regexp/syntax"
	"sort"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/scrub/gitleaksrules"
)

// fixedVendorShape reports whether a rule's secret is a literal vendor prefix of at least three
// characters followed by a body of one fixed length of at least eight characters: no open or
// ranged repeat, no optional part.
func fixedVendorShape(r gitleaksrules.Rule) bool {
	re, err := syntax.Parse(r.Regex, syntax.Perl)
	if err != nil {
		return false
	}
	re = re.Simplify()
	n := secretNode(re, r.SecretGroup)
	if n == nil {
		n = re
	}
	n = trimAssertions(n)
	l, ok := fixedLen(n)
	p := literalPrefix(n)
	return ok && p >= 3 && l-p >= 8
}

// secretNode returns the capture group that holds the secret: group, or the first one when group
// is zero (the way the scrubber and gitleaks pick it).
func secretNode(re *syntax.Regexp, group int) *syntax.Regexp {
	var found *syntax.Regexp
	var walk func(*syntax.Regexp)
	walk = func(n *syntax.Regexp) {
		if found != nil {
			return
		}
		if n.Op == syntax.OpCapture && (n.Cap == group || group == 0) {
			found = n.Sub[0]
			return
		}
		for _, s := range n.Sub {
			walk(s)
		}
	}
	walk(re)
	return found
}

func isAssertion(n *syntax.Regexp) bool {
	switch n.Op {
	case syntax.OpBeginText, syntax.OpEndText, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpWordBoundary, syntax.OpEmptyMatch:
		return true
	}
	return false
}

// trimAssertions drops the zero-width assertions at both ends of a concatenation.
func trimAssertions(n *syntax.Regexp) *syntax.Regexp {
	if n.Op != syntax.OpConcat {
		return n
	}
	sub := n.Sub
	for len(sub) > 0 && isAssertion(sub[0]) {
		sub = sub[1:]
	}
	for len(sub) > 0 && isAssertion(sub[len(sub)-1]) {
		sub = sub[:len(sub)-1]
	}
	if len(sub) == 1 {
		return sub[0]
	}
	c := *n
	c.Sub = sub
	return &c
}

// fixedLen returns the one length every match of n has, if there is one.
func fixedLen(n *syntax.Regexp) (int, bool) {
	switch n.Op {
	case syntax.OpLiteral:
		return len(n.Rune), true
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1, true
	case syntax.OpCapture:
		return fixedLen(n.Sub[0])
	case syntax.OpRepeat:
		if n.Min != n.Max {
			return 0, false
		}
		l, ok := fixedLen(n.Sub[0])
		return l * n.Min, ok
	case syntax.OpConcat:
		t := 0
		for _, s := range n.Sub {
			if isAssertion(s) {
				continue
			}
			l, ok := fixedLen(s)
			if !ok {
				return 0, false
			}
			t += l
		}
		return t, true
	case syntax.OpAlternate:
		first := -1
		for _, s := range n.Sub {
			l, ok := fixedLen(s)
			if !ok || (first >= 0 && l != first) {
				return 0, false
			}
			first = l
		}
		return first, true
	}
	return 0, false
}

// literalPrefix returns how many leading characters of every match of n are fixed: a literal,
// or a class of at most four characters (the parser turns ghu|ghs into gh[su]).
func literalPrefix(n *syntax.Regexp) int {
	switch n.Op {
	case syntax.OpLiteral:
		return len(n.Rune)
	case syntax.OpCharClass:
		w := 0
		for i := 0; i+1 < len(n.Rune); i += 2 {
			w += int(n.Rune[i+1]-n.Rune[i]) + 1
		}
		if w <= 4 {
			return 1
		}
		return 0
	case syntax.OpCapture:
		return literalPrefix(n.Sub[0])
	case syntax.OpConcat:
		t := 0
		for _, s := range n.Sub {
			if isAssertion(s) {
				continue
			}
			p := literalPrefix(s)
			t += p
			if l, ok := fixedLen(s); !ok || p < l {
				return t
			}
		}
		return t
	case syntax.OpAlternate:
		least := -1
		for _, s := range n.Sub {
			if p := literalPrefix(s); least < 0 || p < least {
				least = p
			}
		}
		return least
	}
	return 0
}

// keepFloor lists the fixed-shape rules that keep their entropy floor, with the reason.
var keepFloor = map[string]string{
	// The prefix is the base64 of ordinary text ("ref"), not a vendor marker: any base64 of text
	// that starts that way has the shape, and the floor is what tells a token apart.
	"artifactory-reference-token": "base64 prefix of ordinary text",
}

// TestNoFloorRulesAreFixedVendorShapes keeps the list of rules applied without an entropy floor
// in step with the pinned gitleaks set: it fails when a bump renames or drops a listed rule, takes
// its floor away, or changes its expression so the secret is no longer a fixed vendor shape, and
// when a bump adds a fixed-shape rule with a floor that is not listed (review it, then list it).
func TestNoFloorRulesAreFixedVendorShapes(t *testing.T) {
	set, err := gitleaksrules.Load()
	if err != nil {
		t.Fatal(err)
	}
	listed := scrub.GitleaksNoFloor()
	byID := map[string]gitleaksrules.Rule{}
	for _, r := range set.Rules {
		byID[r.ID] = r
	}
	check := func(id string) {
		r, ok := byID[id]
		switch {
		case !ok:
			t.Errorf("%s: listed but not in gitleaks %s: renamed or dropped", id, set.Version)
		case r.Entropy == 0:
			t.Errorf("%s: has no entropy floor in gitleaks %s: remove it from its list", id, set.Version)
		case !fixedVendorShape(r):
			t.Errorf("%s: no longer a fixed vendor prefix with a fixed-length body: %s", id, r.Regex)
		}
	}
	for id := range listed {
		check(id)
	}
	for id := range keepFloor {
		if _, ok := listed[id]; ok {
			t.Errorf("%s: both listed without a floor and in keepFloor", id)
		}
		check(id) // a stale keepFloor entry fails like a stale list entry
	}
	var missing []string
	for _, r := range set.Rules {
		if _, ok := listed[r.ID]; ok || r.Entropy == 0 || !fixedVendorShape(r) {
			continue
		}
		if _, ok := scrub.GitleaksExcluded()[r.ID]; ok {
			continue
		}
		if _, ok := keepFloor[r.ID]; ok {
			continue
		}
		missing = append(missing, r.ID)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fixed vendor shapes with an entropy floor that are not listed: %s", strings.Join(missing, ", "))
	}
}

// TestFixedVendorShapesBelowTheFloor: a key of a fixed vendor shape whose characters repeat
// enough to fall under the rule's entropy floor is still a key in a trace. FuzzAlwaysShapesNeverSurvive
// found an AWS key ID of that kind.
func TestFixedVendorShapesBelowTheFloor(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for name, key := range map[string]string{
		"aws key id, two letters": "AKIA" + strings.Repeat("QZ", 8),
		"aws key id, one letter":  "AKIA" + strings.Repeat("Q", 16),
		"github pat":              "ghp_" + strings.Repeat("ab", 18),
		"github app":              "ghs_" + strings.Repeat("z", 36),
		"gitlab pat":              "glpat-" + strings.Repeat("x", 20),
		"npm":                     "npm_" + strings.Repeat("a", 36),
	} {
		for _, in := range []string{key, "id " + key + " ok", `{"k":"` + key + `"}`, "x/" + key} {
			if out := s.Text(in); strings.Contains(out, key) {
				t.Errorf("%s: survived in %q: %q", name, in, out)
			}
		}
	}
}
