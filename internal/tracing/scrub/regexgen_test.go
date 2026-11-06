// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/scrub/gitleaksrules"
)

// The tests of the gitleaks rules need a text each rule matches. gitleaks keeps such samples in
// the Go functions that define its rules, and copying them into this repository would commit
// hundreds of key-shaped strings (secret scanners flag them, rightly). So the samples are made at
// run time from each rule's regular expression, by the generator below. It is deterministic: the
// seed of an attempt is a function of the rule ID and the attempt number, so a failure reproduces.

// regexGen builds a string that a regular expression matches, choosing at random wherever the
// expression allows a choice. Anchors and word boundaries produce nothing; the caller checks the
// result against the real rule and tries again when a boundary or a lookaround-like constraint
// (gitleaks' entropy floor, its allowlists) rejects it.
type regexGen struct {
	rng *rand.Rand
	// long makes repetitions take their upper bound (capped) instead of a random count.
	long bool
}

// printable is the part of ASCII a generated character comes from (no space, no control byte).
const (
	printLo = 0x21
	printHi = 0x7e
)

func (g *regexGen) classRune(ranges []rune) rune {
	type span struct{ lo, hi rune }
	var spans []span
	total := 0
	add := func(lo, hi rune) {
		for i := 0; i+1 < len(ranges); i += 2 {
			a, b := max(ranges[i], lo), min(ranges[i+1], hi)
			if a <= b {
				spans = append(spans, span{a, b})
				total += int(b-a) + 1
			}
		}
	}
	add(printLo, printHi)
	if total == 0 {
		add(' ', ' ')
	}
	if total == 0 {
		add('\t', '\n')
	}
	if total == 0 {
		return 'a'
	}
	n := g.rng.IntN(total)
	for _, s := range spans {
		if size := int(s.hi-s.lo) + 1; n < size {
			return s.lo + rune(n)
		} else {
			n -= size
		}
	}
	return 'a'
}

func (g *regexGen) count(lo, hi int) int {
	capHi := lo + 16
	if g.long {
		capHi = lo + 64
	}
	if hi < 0 || hi > capHi {
		hi = capHi
	}
	if g.long {
		return hi
	}
	return lo + g.rng.IntN(hi-lo+1)
}

func (g *regexGen) emit(sb *strings.Builder, re *syntax.Regexp) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			sb.WriteRune(r)
		}
	case syntax.OpCharClass:
		sb.WriteRune(g.classRune(re.Rune))
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		sb.WriteRune(g.classRune([]rune{printLo, printHi}))
	case syntax.OpCapture:
		g.emit(sb, re.Sub[0])
	case syntax.OpConcat:
		for _, s := range re.Sub {
			g.emit(sb, s)
		}
	case syntax.OpAlternate:
		g.emit(sb, re.Sub[g.rng.IntN(len(re.Sub))])
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		lo, hi := re.Min, re.Max
		switch re.Op {
		case syntax.OpStar:
			lo, hi = 0, -1
		case syntax.OpPlus:
			lo, hi = 1, -1
		case syntax.OpQuest:
			lo, hi = 0, 1
		}
		for n := g.count(lo, hi); n > 0; n-- {
			g.emit(sb, re.Sub[0])
		}
	case syntax.OpNoMatch:
		panic("regexgen: expression matches nothing")
	default:
		// Empty match, anchors and word boundaries: nothing to write.
	}
}

// generateFrom returns a string the expression may match, for one seed.
func generateFrom(expr string, seed1, seed2 uint64, long bool) (string, error) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return "", err
	}
	g := &regexGen{rng: rand.New(rand.NewPCG(seed1, seed2)), long: long}
	var sb strings.Builder
	g.emit(&sb, re)
	return sb.String(), nil
}

func ruleSeed(id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

func TestRegexGenMatchesItsExpression(t *testing.T) {
	for _, expr := range []string{
		`ab+c`, `(?i)x[a-f0-9]{8,12}y`, `\b(?:foo|bar)[_-]?[A-Z]{3}\d{2}\b`, `^z.{4}$`, `(q)(?:r|s){2}`,
		`[^a-z\s]{5}`, `\w{3}\.\w{3}`,
	} {
		re := regexp.MustCompile(expr)
		for seed := range uint64(20) {
			for _, long := range []bool{false, true} {
				got, err := generateFrom(expr, 1, seed, long)
				if err != nil {
					t.Fatal(err)
				}
				if !re.MatchString(got) {
					t.Errorf("%s: generated %q does not match", expr, got)
				}
			}
		}
	}
}

// sampleSkips lists the rules for which no sample can be generated, with the reason. A sample is
// good when the rule matches it alone (keywords, entropy floor and allowlists included) and the
// scrubber redacts it. The test fails for a rule that is neither sampled nor listed, and for a
// listed one that is sampled now, so the list cannot go stale.
var sampleSkips = map[string]string{}

type sample struct {
	text string
	err  string
}

var (
	samplesOnce sync.Once
	samplesAll  map[string]sample
)

// generatedSamples returns, for every rule of the pinned set that the scrubber applies, one
// generated sample or the reason there is none.
func generatedSamples(t *testing.T) map[string]sample {
	t.Helper()
	samplesOnce.Do(func() {
		set, err := gitleaksrules.Load()
		if err != nil {
			t.Error(err)
			return
		}
		samplesAll = map[string]sample{}
		s := scrub.New(scrub.Config{Level: scrub.LevelFull})
		excluded := scrub.GitleaksExcluded()
		for _, r := range set.Rules {
			if _, ok := excluded[r.ID]; ok {
				continue
			}
			samplesAll[r.ID] = makeSample(s, r)
		}
	})
	if samplesAll == nil {
		t.FailNow()
	}
	return samplesAll
}

func makeSample(s *scrub.Scrubber, r gitleaksrules.Rule) sample {
	const attempts = 400
	for i := range uint64(attempts) {
		text, err := generateFrom(r.Regex, ruleSeed(r.ID), i, i%2 == 1)
		if err != nil {
			return sample{err: err.Error()}
		}
		variants := []string{text}
		// A rule with a loose expression and a keyword needs the keyword in the text; most rules
		// contain theirs already.
		if len(r.Keywords) > 0 {
			variants = append(variants, r.Keywords[int(i)%len(r.Keywords)]+" "+text)
		}
		for _, v := range variants {
			ids, skipped := scrub.GitleaksMatches(v)
			if !slices.Contains(ids, r.ID) && !slices.Contains(skipped, r.ID) {
				continue
			}
			// The rule's own keyword must be in the text, found here without the pre-filter.
			low := strings.ToLower(v)
			hasKW := len(r.Keywords) == 0
			for _, k := range r.Keywords {
				hasKW = hasKW || strings.Contains(low, k)
			}
			if hasKW && s.Text(v) != v {
				return sample{text: v}
			}
		}
	}
	return sample{err: fmt.Sprintf("no generated text of %d attempts is matched by the rule and redacted", attempts)}
}

func TestGeneratedSamplesCoverTheRules(t *testing.T) {
	samples := generatedSamples(t)
	missing := 0
	for id, sm := range samples {
		_, listed := sampleSkips[id]
		switch {
		case sm.text == "" && !listed:
			t.Errorf("%s: no sample (%s); fix the generator or list the rule in sampleSkips with the reason", id, sm.err)
		case sm.text != "" && listed:
			t.Errorf("%s: has a sample now: remove it from sampleSkips", id)
		}
		if sm.text == "" {
			missing++
		}
	}
	for id := range sampleSkips {
		if _, ok := samples[id]; !ok {
			t.Errorf("sampleSkips lists %s, which is not a rule of the set", id)
		}
	}
	t.Logf("%d rules, %d without a generated sample", len(samples), missing)
	if len(samples) < 100 || missing*10 > len(samples) {
		t.Fatalf("%d rules, %d without a sample", len(samples), missing)
	}
}

// Every generated sample is redacted at full.
func TestGitleaksGeneratedSamplesRedacted(t *testing.T) {
	s := scrub.New(scrub.Config{Level: scrub.LevelFull})
	for id, sm := range generatedSamples(t) {
		if sm.text == "" {
			continue
		}
		if got := s.Text(sm.text); got == sm.text {
			t.Errorf("%s: sample passed at full", id)
		}
	}
}

// The keyword pre-filter only skips rules that cannot match: a rule's own sample is found through
// the pre-filter.
func TestGitleaksKeywordPrefilterIsSound(t *testing.T) {
	for id, sm := range generatedSamples(t) {
		if sm.text != "" && scrub.GitleaksPrefilterSkips(id, sm.text) {
			t.Errorf("%s: the keyword pre-filter skipped the rule's own sample", id)
		}
	}
}
