// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	if err := m.clock(); err != nil {
		return budgetDecision(err), nil
	}
	lctx, cancel := context.WithDeadline(ctx, m.deadline)
	res, err := c.Store.Lookup(lctx, scope, all)
	cancel()
	if err == nil {
		err = m.clock()
		if err != nil {
			return budgetDecision(err), nil
		}
	}
	hits := res.Found
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
		d := allow(ReasonPass)
		d.Evicted = res.Evicted
		return d, nil
	}
	d := block(ReasonTaint, "the call would send %d characters of private text; it needs approval", best.Length)
	d.Match = best
	d.Evicted = res.Evicted
	return d, nil
}

// maxPairs bounds the ordered argument pairs of one call, as a multiple of MaxPairArgs squared.
const maxPairs = 16

// minPairJoin is the least bytes of each side joined for split reassembly: a leak is at least the
// threshold (24 canonical characters), so a boundary needs that much around it, and hex is two
// bytes to a character.
const minPairJoin = 64

// pairBudgetDiv sets the joined text of all pairs of one call, before decoding, to
// MaxWorkBytes/pairBudgetDiv (2 MiB by default). Plain text of that size is decoded and
// fingerprinted in about 0.1 s on a laptop (about 0.05 ms per KiB, measured on the call
// shapes of evasion_test.go); base64 of it costs three times that. A call of 128
// arguments of 40 bytes, 128*127 pairs of 80 bytes, is 1.3 MiB.
const pairBudgetDiv = 16

// pairJoin returns how many bytes of each side of an argument pair to join for the arguments idx,
// or false when the pairs cannot be checked: more than maxPairs*MaxPairArgs^2 pairs, or joined
// text over the budget even at minPairJoin. The join starts at PairJoinBytes and halves until the
// pairs fit the budget, so a call with long arguments is checked with less reach per boundary
// instead of running out of time. Up to MaxPairArgs arguments are always checked, at minPairJoin
// if need be; more are checked only within the budget, and a call too big for it fails closed.
func pairJoin(args []string, idx []int, lim Limits) (int, bool) {
	n := len(idx)
	many := n > lim.MaxPairArgs
	if many && n*(n-1) > maxPairs*lim.MaxPairArgs*lim.MaxPairArgs {
		return 0, false
	}
	floor := min(minPairJoin, lim.PairJoinBytes)
	for join := lim.PairJoinBytes; ; join = max(join/2, floor) {
		bytes := 0
		for _, i := range idx {
			bytes += (n - 1) * (min(len(args[i]), join) * 2)
		}
		if bytes <= lim.MaxWorkBytes/pairBudgetDiv {
			return join, true
		}
		if join <= floor {
			return floor, !many
		}
	}
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
	lo    int // the first k-gram of the text that fps holds (a pair holds those across its cut)
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
	n := len(raws)
	idx := make([]int, 0, n)
	for _, r := range raws {
		idx = append(idx, r.args[0])
	}
	var join int
	if n > 1 {
		var b strings.Builder
		for _, r := range raws {
			// An argument is decoded whole on its own; in the joined text a long one is cut to
			// its two ends, which hold the boundaries to its neighbours. Joined whole, a call of
			// two halves of 1 MiB would cost twice a call of one.
			if w := lim.PairJoinBytes; len(r.text) > 2*w {
				b.WriteString(head(r.text, w))
				b.WriteString(tail(r.text, w))
			} else {
				b.WriteString(r.text)
			}
		}
		raws = append(raws, rawText{b.String(), idx})
		var fits bool
		if join, fits = pairJoin(args, idx, lim); !fits {
			return nil, &budgetError{ReasonBudgetArgs, fmt.Sprintf("the call has %d non-empty arguments, too many to check every pair of them for split text; the tool call could not be checked", n)}
		}
	}
	res, err := decodeRaws(raws, lim, fp, m)
	if err != nil {
		return nil, err
	}
	if n > 1 {
		// The pairs: tail(ai)+head(aj), i != j, the in-order boundaries being in the joined text.
		// Up to MaxPairArgs arguments, the joined strings are decoded again, so a stream cut
		// anywhere decodes whole. Above that, decoding every pair costs a call of 128 arguments
		// seconds on one core, and the pairs are built from the decoded forms of the arguments
		// instead (see pairForms).
		if n <= lim.MaxPairArgs {
			var pairs []rawText
			for i := range n {
				for j := range n {
					if i != j && j != i+1 {
						pairs = append(pairs, rawText{tail(args[idx[i]], join) + head(args[idx[j]], join), []int{idx[i], idx[j]}})
					}
				}
			}
			more, err := decodeRaws(pairs, lim, fp, m)
			if err != nil {
				return nil, err
			}
			res = append(res, more...)
		} else {
			forms := make([][]string, n)
			for i := range n {
				forms[i] = pairForms(res[i].vars)
			}
			var more decoded
			// A pair is only fingerprinted: one buffer serves the canonical forms, and the
			// fingerprints and argument numbers of all pairs share a few large slabs.
			var scratch Canonical
			var fpSlab []Fingerprint
			var argSlab []int
			// emit adds the text of a pair of ends, joined at byte cut, if its canonical form
			// holds a k-gram across the cut; only the k-grams across it are fingerprinted, the
			// ones inside an end being those of the argument itself.
			emit := func(i, j int, text string, cut int, chain string) error {
				if err := m.add(len(text)); err != nil {
					return err
				}
				scratch.Runes, scratch.Off = scratch.Runes[:0], scratch.Off[:0]
				lo, hi := 0, 0
				if cut < 0 { // the cut is not known: every k-gram
					normaliseInto(&scratch, text)
					hi = len(scratch.Runes) - fp.k
				} else {
					normaliseInto(&scratch, text[:cut])
					nb := len(scratch.Runes)
					normaliseInto(&scratch, text[cut:]) // offsets of the second part are relative to it
					lo, hi = max(0, nb-fp.k+1), min(nb-1, len(scratch.Runes)-fp.k)
				}
				if hi < lo {
					return nil
				}
				if need := hi - lo + 1; cap(fpSlab)-len(fpSlab) < need {
					fpSlab = make([]Fingerprint, 0, max(need, 1<<14))
				}
				from := len(fpSlab)
				fpSlab = fp.kgrams(scratch.Runes[lo:hi+fp.k], fpSlab)
				if cap(argSlab)-len(argSlab) < 2 {
					argSlab = make([]int, 0, 1<<10)
				}
				argSlab = append(argSlab, idx[i], idx[j])
				if len(more.cands) == cap(more.cands) { // chunks: a slice grown by doubling copies them all
					if len(more.cands) > 0 {
						res = append(res, more)
					}
					more = decoded{cands: make([]keyedCandidate, 0, 2048)}
				}
				more.cands = append(more.cands, keyedCandidate{candidate{args: argSlab[len(argSlab)-2:], v: Variant{Text: text, Chain: chain}, fps: fpSlab[from:len(fpSlab):len(fpSlab)], lo: lo}, text})
				return nil
			}
			// The cut of an encoded stream falls anywhere, so where the ends facing each
			// other are both runs of base64 (or base32, hex) characters, a window around
			// the boundary is decoded as the stream it would be joined.
			tails, heads := make([]string, n), make([]string, n)
			nt, nh := 0, 0
			for i := range n {
				tails[i], heads[i] = tailRun(args[idx[i]]), headRun(args[idx[i]])
				if len(tails[i]) >= minWindowRun {
					nt++
				}
				if len(heads[i]) >= minWindowRun {
					nh++
				}
			}
			// A window costs about 15 us on one core; more pairs than maxWindowPairs would take
			// the call past its clock, so such a call (about 90 arguments that all begin and end in
			// base64 text) has no windows and relies on the decoded forms alone (a known limit).
			windows := nt*nh <= maxWindowPairs
			for i := range n {
				for j := range n {
					if i == j || j == i+1 {
						continue
					}
					if windows && len(tails[i]) >= minWindowRun && len(heads[j]) >= minWindowRun {
						w := tails[i] + heads[j]
						if err := m.add(len(w)); err != nil {
							return nil, err
						}
						for _, text := range windowReadings(w) {
							if err := emit(i, j, text, -1, "window"); err != nil {
								return nil, err
							}
						}
					}
					for _, fi := range forms[i] {
						for _, fj := range forms[j] {
							ti := tail(fi, formReach)
							if err := emit(i, j, ti+head(fj, formReach), len(ti), "pair"); err != nil {
								return nil, err
							}
						}
					}
				}
			}
			res = append(res, more)
		}
	}
	seen := map[string]struct{}{}
	var out []candidate
	for _, rs := range res {
		for _, c := range rs.cands {
			if _, dup := seen[c.key]; dup {
				continue
			}
			seen[c.key] = struct{}{}
			out = append(out, c.candidate)
		}
	}
	return out, nil
}

// formReach is the bytes of each side of a boundary taken from the decoded forms of the arguments
// of a call of many: a half of a leak is under 24 canonical characters or it is caught alone, so
// a side needs about that many, and 40 bytes leaves room for the punctuation and spaces between.
const formReach = 40

// maxWindowPairs bounds the boundaries of a call whose windows are decoded.
const maxWindowPairs = 8000

// windowRun is the most characters of each side of a boundary that are decoded as one stream,
// and minWindowRun the least of each side for a window to be decoded: a leak of 24 characters
// cut in two leaves one side more than a few characters.
const (
	windowRun    = 48
	minWindowRun = 4
)

// tailRun and headRun are the last and first windowRun bytes of s that are base64 characters
// (either alphabet), up to the first byte that is not.
func tailRun(s string) string {
	i := len(s)
	for i > 0 && len(s)-i < windowRun && isBase64Char(s[i-1]) {
		i--
	}
	return s[i:]
}

func headRun(s string) string {
	i := 0
	for i < len(s) && i < windowRun && isBase64Char(s[i]) {
		i++
	}
	return s[:i]
}

// windowReadings (a window holds no junk between the symbols of a stream, so no resynchronised reading) are the texts a window of base64 characters decodes to as hex, base32 or
// base64, whichever alphabets it fits.
func windowReadings(w string) []string {
	if allHex(w) {
		return hexReadings(w, false).all()
	}
	var out []string
	up := []byte(strings.ToUpper(w))
	if slices.IndexFunc(up, func(c byte) bool { return !isBase32Char(c) }) < 0 {
		out = append(out, b32Readings(up, false).all()...)
	}
	buf := []byte(w)
	for i, c := range buf {
		switch c {
		case '-':
			buf[i] = '+'
		case '_':
			buf[i] = '/'
		}
	}
	return append(out, b64Readings(buf, false).all()...)
}

// pairForms returns the texts of an argument that its pairs are built from: the argument itself
// (a plain half of a split, whatever its other part decodes to) and the one of its decodings
// that went deepest.
func pairForms(vs []Variant) []string {
	depth := func(v Variant) int {
		if v.Chain == "" {
			return 0
		}
		return strings.Count(v.Chain, ">") + 1
	}
	deepest := 0
	for _, v := range vs {
		deepest = max(deepest, depth(v))
	}
	out := []string{vs[0].Text}
	for _, v := range vs {
		if deepest > 0 && depth(v) == deepest && len(out) < 2 && !slices.Contains(out, v.Text) {
			out = append(out, v.Text)
		}
	}
	return out
}

type keyedCandidate struct {
	candidate
	key string
}

// decoded is what one text decodes to: the variants, and those of them long enough to match.
type decoded struct {
	vars  []Variant
	cands []keyedCandidate
}

// decodeRaws decodes, normalises and fingerprints each text. The texts are independent and a
// call may carry thousands of them (the pairs of its arguments), so several workers take them in
// turn, as a speed-up only (a pod with one core decodes them in turn too); the results stay in
// the order of raws.
func decodeRaws(raws []rawText, lim Limits, fp *Fingerprinter, m *meter) ([]decoded, error) {
	one := func(r rawText) (decoded, error) {
		vs, err := decodeAll(r.text, lim, m)
		if err != nil {
			return decoded{}, err
		}
		out := decoded{vars: vs}
		for _, v := range vs {
			if err := m.add(len(v.Text)); err != nil {
				return decoded{}, err
			}
			c := Normalise(v.Text)
			if len(c.Runes) < fp.k {
				continue
			}
			fps := fp.kgrams(c.Runes, make([]Fingerprint, 0, len(c.Runes)-fp.k+1))
			out.cands = append(out.cands, keyedCandidate{candidate{args: r.args, v: v, canon: c, fps: fps}, c.String()})
		}
		return out, nil
	}
	res := make([]decoded, len(raws))
	workers := min(runtime.GOMAXPROCS(0), maxWorkers, len(raws)/minPerWorker)
	if workers <= 1 {
		for i, r := range raws {
			var err error
			if res[i], err = one(r); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	var (
		next   atomic.Int64
		failed atomic.Bool
		wg     sync.WaitGroup
		errs   = make([]error, len(raws))
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= len(raws) {
					return
				}
				var err error
				if res[i], err = one(raws[i]); err != nil {
					errs[i] = err
					failed.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs { // the first in call order, as a serial run reports it
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// maxWorkers and minPerWorker bound the workers of decodeRaws: a call of a few texts is decoded
// in turn, not worth the goroutines.
const (
	maxWorkers   = 8
	minPerWorker = 8
)

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
	if c.canon.Off == nil { // a pair or window candidate keeps its text only, not its canonical form
		c.canon = Normalise(c.v.Text)
	}
	bestStart, bestEnd := -1, -1
	start, end := -1, -1
	for i, h := range hits {
		if !h {
			continue
		}
		i += c.lo
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
