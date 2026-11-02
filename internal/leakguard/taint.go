// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Match is the tainted text found in a call's arguments, for the approval card (ADR 0084
// decision 7).
type Match struct {
	// Args are the indexes of the arguments the match came from: one, or several when the text
	// was split across arguments.
	Args []int
	// Encoding names the decoders that revealed the text ("" when it was sent as is).
	Encoding string
	// Excerpt is the matched text as it reads after decoding, capped at MaxExcerptBytes.
	Excerpt string
	// Length is the number of canonical characters the matched fingerprints span: at least the
	// k-gram length. Fingerprints at most a window apart count as one span.
	Length int
}

// MaxExcerptBytes caps Match.Excerpt.
const MaxExcerptBytes = 512

// TaintChecker checks call arguments against a Store.
type TaintChecker struct {
	Store  Store
	Limits Limits
}

// Check reports whether args, the string arguments of one tool call in call order, carry text
// that scope's store holds. A budget that runs out blocks with its reason (fail closed). A store
// failure blocks with ReasonStoreError and returns the error too.
func (c TaintChecker) Check(ctx context.Context, scope Scope, fp *Fingerprinter, args []string) (Decision, error) {
	lim := c.Limits.withDefaults()
	if fp == nil || c.Store == nil {
		err := errors.New("leakguard: taint check without a store or fingerprinter")
		return block(ReasonStoreError, "the taint store is not available; the tool call could not be checked"), err
	}
	if len(args) > lim.MaxArgs {
		return block(ReasonBudgetArgs, "the call has %d arguments; at most %d can be checked", len(args), lim.MaxArgs), nil
	}
	total := 0
	for _, a := range args {
		total += len(a)
	}
	if total > lim.MaxInputBytes {
		return block(ReasonBudgetInput, "the arguments are %d bytes; at most %d can be checked", total, lim.MaxInputBytes), nil
	}
	m := newMeter(ctx, lim)
	if err := m.clock(); err != nil {
		return budgetDecision(err), nil
	}
	cands, err := candidates(args, lim, fp, m)
	if err != nil {
		return budgetDecision(err), nil
	}
	var all []Fingerprint
	for i := range cands {
		cands[i].first = len(all)
		all = append(all, cands[i].fps...)
	}
	if len(all) == 0 {
		return allow(ReasonPass), nil
	}
	hits, err := c.Store.Lookup(ctx, scope, all)
	if err != nil || len(hits) != len(all) {
		if err == nil {
			err = errors.New("leakguard: the store answered for the wrong number of fingerprints")
		}
		return block(ReasonStoreError, "the taint store failed; the tool call could not be checked"), err
	}
	var best *Match
	for i := range cands {
		if mt := cands[i].match(hits[cands[i].first:cands[i].first+len(cands[i].fps)], fp); mt != nil {
			if best == nil || mt.Length > best.Length {
				best = mt
			}
		}
	}
	if best == nil {
		return allow(ReasonPass), nil
	}
	d := block(ReasonTaint, "the call would send %d characters of private text; it needs approval", best.Length)
	d.Match = best
	return d, nil
}

func budgetDecision(err error) Decision {
	var be *budgetError
	if errors.As(err, &be) {
		return be.decision()
	}
	return block(ReasonBudgetWork, "the tool call could not be checked: %v", err)
}

// candidate is one decoded, normalised text the call carries.
type candidate struct {
	args  []int
	v     Variant
	canon Canonical
	fps   []Fingerprint
	first int
}

type rawText struct {
	text string
	args []int
}

// candidates decodes and fingerprints each argument, the arguments joined in call order, and
// the ordered pairs of arguments (see the package documentation).
func candidates(args []string, lim Limits, fp *Fingerprinter, m *meter) ([]candidate, error) {
	raws := make([]rawText, 0, len(args)+1)
	for i, a := range args {
		if a != "" {
			raws = append(raws, rawText{a, []int{i}})
		}
	}
	if len(raws) > 1 {
		var b strings.Builder
		idx := make([]int, 0, len(raws))
		for _, r := range raws {
			b.WriteString(r.text)
			idx = append(idx, r.args[0])
		}
		raws = append(raws, rawText{b.String(), idx})
		n := len(idx)
		pair := func(i, j int) {
			ai, aj := args[idx[i]], args[idx[j]]
			raws = append(raws, rawText{tail(ai, lim.PairJoinBytes) + head(aj, lim.PairJoinBytes), []int{idx[i], idx[j]}})
		}
		if n <= lim.MaxPairArgs {
			for i := range n {
				for j := range n {
					if i != j && j != i+1 { // the joined text already holds every in-order boundary
						pair(i, j)
					}
				}
			}
		} else {
			for i := 0; i+1 < n; i++ {
				pair(i+1, i)
			}
		}
	}
	seen := map[string]struct{}{}
	var out []candidate
	for _, r := range raws {
		vs, err := decodeAll(r.text, lim, m)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if err := m.add(len(v.Text)); err != nil {
				return nil, err
			}
			c := Normalise(v.Text)
			key := c.String()
			if len(c.Runes) < fp.k {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, candidate{args: r.args, v: v, canon: c, fps: fp.kgrams(c.Runes, nil)})
		}
	}
	return out, nil
}

// head returns at most n bytes from the start of s, cut at a character boundary.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tail returns at most n bytes from the end of s, cut at a character boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// match returns the longest run of hits as a Match, or nil. Hits at most a window apart are one
// run: winnowing keeps at least one fingerprint in every window of the source.
func (c *candidate) match(hits []bool, fp *Fingerprinter) *Match {
	k, w := fp.k, fp.p.Window
	bestStart, bestEnd := -1, -1
	start, end := -1, -1
	for i, h := range hits {
		if !h {
			continue
		}
		if start >= 0 && i <= end-k+w {
			end = i + k
		} else {
			start, end = i, i+k
		}
		if end-start > bestEnd-bestStart {
			bestStart, bestEnd = start, end
		}
	}
	if bestStart < 0 {
		return nil
	}
	from := c.canon.Off[bestStart]
	last := c.canon.Off[bestEnd-1]
	_, size := utf8.DecodeRuneInString(c.v.Text[last:])
	excerpt := strings.ToValidUTF8(c.v.Text[from:last+size], "�")
	if len(excerpt) > MaxExcerptBytes {
		excerpt = head(excerpt, MaxExcerptBytes)
	}
	args := append([]int(nil), c.args...)
	return &Match{Args: args, Encoding: c.v.Chain, Excerpt: excerpt, Length: bestEnd - bestStart}
}

// String describes the match without its text, for logs.
func (m *Match) String() string {
	return fmt.Sprintf("taint match of %d characters in arguments %v (encoding %q)", m.Length, m.Args, m.Encoding)
}
