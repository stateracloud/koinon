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
	return c.check(ctx, scope, fp, args, nil, nil)
}

// CheckJoins is Check, and also scans args joined in each of orders: an order lists indices of
// args, each at most once (a caller's other reading of the same call, such as the canonical order
// of its JSON members, #256). An order equal to call order or to an earlier order, or naming an
// index out of range, adds nothing.
func (c TaintChecker) CheckJoins(ctx context.Context, scope Scope, fp *Fingerprinter, args []string, orders ...[]int) (Decision, error) {
	return c.check(ctx, scope, fp, args, orders, nil)
}

// check is CheckJoins, and also stores in work, when it is not nil, the bytes of work the call was
// charged (the figure the work caps of the specs read, see export_test.go).
func (c TaintChecker) check(ctx context.Context, scope Scope, fp *Fingerprinter, args []string, orders [][]int, work *int64) (Decision, error) {
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
	if work != nil {
		defer func() { *work = m.used.Load() }()
	}
	if err := m.clock(); err != nil {
		return budgetDecision(err), nil
	}
	cands, err := candidates(args, orders, lim, fp, m)
	if err != nil {
		return budgetDecision(err), nil
	}
	nfp := 0
	for i := range cands {
		nfp += len(cands[i].fps)
	}
	all := make([]Fingerprint, 0, nfp)
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

// candidates decodes and fingerprints each argument, the arguments joined in call order, and the
// arguments joined in each of orders (see the package documentation): nothing else. A text split
// across arguments that are not adjacent in one of those orders is not reassembled (ADR 0091
// decision 6: the text match is a secondary signal).
func candidates(args []string, orders [][]int, lim Limits, fp *Fingerprinter, m *meter) ([]candidate, error) {
	raws := make([]rawText, 0, len(args)+1)
	for i, a := range args {
		if a != "" {
			raws = append(raws, rawText{a, []int{i}})
		}
	}
	callOrder := make([]int, 0, len(raws))
	for _, r := range raws {
		callOrder = append(callOrder, r.args[0])
	}
	if j, ok := join(args, callOrder, lim.JoinEndBytes); ok {
		raws = append(raws, j)
	}
	extra := extraOrders(args, orders, callOrder)
	res, err := decodeRaws(raws, lim, fp, m)
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		// The extra orders are the same arguments again, so each costs about as much as the
		// call-order join (up to a third more of the call's work, with the MCP guard's three).
		// A call whose arguments and call-order join already took a quarter of the work budget
		// joins them with ends of a quarter of JoinEndBytes: still the boundaries between the
		// arguments (128 bytes each side by default, more than a threshold-length leak in three
		// layers of base64 of UTF-16), at a quarter of the cost. The orders are never dropped:
		// dropping them under load would let a padded call split text in key order unseen.
		ends := lim.JoinEndBytes
		if m.used.Load() > int64(lim.MaxWorkBytes/heavyWorkShare) {
			ends = max(lim.JoinEndBytes/heavyWorkShare, 1)
		}
		jr := make([]rawText, 0, len(extra))
		for _, idx := range extra {
			if j, ok := join(args, idx, ends); ok {
				jr = append(jr, j)
			}
		}
		more, err := decodeRaws(jr, lim, fp, m)
		if err != nil {
			return nil, err
		}
		res = append(res, more...)
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

// heavyWorkShare: a call that has used more than 1/heavyWorkShare of MaxWorkBytes before its
// extra orders joins them with ends of JoinEndBytes/heavyWorkShare (see candidates).
const heavyWorkShare = 4

// extraOrders returns the orders to join besides call order: each valid order once, without its
// empty arguments. An order that names an index twice or out of range is skipped, and so is one
// that call order or an earlier order already gave: it would only charge the work budget again for
// the same text.
func extraOrders(args []string, orders [][]int, callOrder []int) [][]int {
	done := [][]int{callOrder}
	var out [][]int
	for _, o := range orders {
		idx := make([]int, 0, len(o))
		used := make(map[int]bool, len(o))
		valid := true
		for _, i := range o {
			if i < 0 || i >= len(args) || used[i] {
				valid = false
				break
			}
			used[i] = true
			if args[i] != "" {
				idx = append(idx, i)
			}
		}
		if !valid || slices.ContainsFunc(done, func(d []int) bool { return slices.Equal(d, idx) }) {
			continue
		}
		done = append(done, idx)
		out = append(out, idx)
	}
	return out
}

// join is the non-empty arguments idx names, joined in that order, each longer than 2*ends cut to
// its two ends; false for fewer than two.
func join(args []string, idx []int, ends int) (rawText, bool) {
	if len(idx) < 2 {
		return rawText{}, false
	}
	var b strings.Builder
	for _, i := range idx {
		t := args[i]
		// An argument is decoded whole on its own; in the joined text a long one is cut to its two
		// ends, which hold the boundaries to its neighbours. Joined whole, a call of two halves of
		// 1 MiB would cost twice a call of one.
		if len(t) > 2*ends {
			b.WriteString(head(t, ends))
			b.WriteString(tail(t, ends))
		} else {
			b.WriteString(t)
		}
	}
	return rawText{b.String(), slices.Clone(idx)}, true
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
// call may carry many of them (one per argument), so several workers take them in
// turn, as a speed-up only (a pod with one core decodes them in turn too); the results stay in
// the order of raws.
var canonPool = sync.Pool{New: func() any { return new(Canonical) }}

func decodeRaws(raws []rawText, lim Limits, fp *Fingerprinter, m *meter) ([]decoded, error) {
	one := func(r rawText) (decoded, error) {
		vs, err := decodeAll(r.text, lim, m)
		if err != nil {
			return decoded{}, err
		}
		out := decoded{vars: vs}
		// one buffer for the variants of every text; a match recomputes its own canonical form
		cp := canonPool.Get().(*Canonical)
		defer canonPool.Put(cp)
		c := cp
		for _, v := range vs {
			if err := m.add(len(v.Text)); err != nil {
				return decoded{}, err
			}
			c.Runes, c.Off = c.Runes[:0], c.Off[:0]
			normaliseInto(c, v.Text)
			if len(c.Runes) < fp.k {
				continue
			}
			fps := fp.kgrams(c.Runes, make([]Fingerprint, 0, len(c.Runes)-fp.k+1))
			out.cands = append(out.cands, keyedCandidate{candidate{args: r.args, v: v, fps: fps}, c.String()})
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
	if c.canon.Off == nil { // a candidate keeps its text only, not its canonical form
		c.canon = Normalise(c.v.Text)
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
