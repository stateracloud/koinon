// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// RuleSetVersion names the version of the rules below. It is recorded with every auto-applied
// platform version. Change it whenever a bound or a rule changes.
const RuleSetVersion = "1"

// The bounds of ADR 0098 decision 3. They are constants: configuration can only tighten
// MaxChangePercent and MaxAutoPerDay (Bounds.Validate), never loosen them or touch the rest.
const (
	// DefaultMaxChangePercent is how far a rate may move from the last person-reviewed rate for the
	// change to apply by itself.
	DefaultMaxChangePercent = 25
	// DefaultMaxAutoPerDay is how many entries may be auto-applied in a day.
	DefaultMaxAutoPerDay = 10
	// ConfirmFactor: a rate that moves to more than this many times, or less than one over this
	// many times, the current rate needs a typed confirmation.
	ConfirmFactor = 3
	// UnitMixupFactor: a ratio of this much or more either way suggests per-token against
	// per-million (10^6) or per-thousand (10^3) prices were mixed up.
	UnitMixupFactor = 100
	// HighRateUSDPerMillion: any rate above this needs a typed confirmation.
	HighRateUSDPerMillion = 1000
	// StabilityWindow: a live source's value counts only after two fetches this far apart returned it.
	StabilityWindow = 24 * time.Hour
)

// Bounds are the tunable bounds of auto-apply.
type Bounds struct {
	MaxChangePercent int
	MaxAutoPerDay    int
}

// DefaultBounds are the bounds confirmed for ADR 0098.
var DefaultBounds = Bounds{MaxChangePercent: DefaultMaxChangePercent, MaxAutoPerDay: DefaultMaxAutoPerDay}

// Validate refuses a bound looser than the default, or one below 1.
func (b Bounds) Validate() error {
	var errs []error
	if b.MaxChangePercent < 1 || b.MaxChangePercent > DefaultMaxChangePercent {
		errs = append(errs, fmt.Errorf("the largest automatic price change must be between 1 and %d percent, not %d: configuration can tighten the bound, never loosen it", DefaultMaxChangePercent, b.MaxChangePercent))
	}
	if b.MaxAutoPerDay < 1 || b.MaxAutoPerDay > DefaultMaxAutoPerDay {
		errs = append(errs, fmt.Errorf("the most automatic changes per day must be between 1 and %d, not %d: configuration can tighten the bound, never loosen it", DefaultMaxAutoPerDay, b.MaxAutoPerDay))
	}
	return errors.Join(errs...)
}

// Class of a proposal (ADR 0098 decision 3).
type Class string

// Proposal classes.
const (
	// ClassAuto may be applied by the scheduled fetch.
	ClassAuto Class = "auto"
	// ClassNeedsReview waits for a person.
	ClassNeedsReview Class = "needs_review"
	// ClassConfirm waits for a person who types the model id.
	ClassConfirm Class = "confirm"
	// ClassRefused cannot be accepted: its data does not form a valid entry.
	ClassRefused Class = "refused"
)

// Buckets are the five rates of a price, in the order the rules and messages use.
var Buckets = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

func wireBucket(w pricing.Wire, bucket string) string {
	switch bucket {
	case "input":
		return w.Input
	case "output":
		return w.Output
	case "cache_read":
		return w.CacheRead
	case "cache_write":
		return w.CacheWrite
	default:
		return w.Reasoning
	}
}

// Change is one rate that differs between the current price and the proposed one.
type Change struct {
	Bucket string `json:"bucket"`
	// ContextOver is the tier the rate belongs to; 0 is the base rate.
	ContextOver int64  `json:"contextOver,omitempty"`
	From        string `json:"from,omitempty"` // empty: the bucket is added
	To          string `json:"to,omitempty"`   // empty: the bucket is removed
}

// SameRate compares two rates numerically ("0.50" equals "0.5"); an empty one is unset.
func SameRate(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	ra, ea := money.ParseRate(a)
	rb, eb := money.ParseRate(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ra == rb
}

// billedReasoning spells out the reasoning rate a spec bills where it leaves it unset. Reasoning
// tokens without a rate of their own bill at the output rate (pricing.Price), and a source that
// charges reasoning at the output rate drops the bucket (separateReasoning), so "reasoning equals
// output" and "no reasoning rate" are one price and must compare equal. The base rate is the
// output rate; a tier that sets no reasoning rate inherits the base one when the base sets it, and
// otherwise bills at its own output rate when it sets one (and at the base's when not, which the
// filled-in base rate already says).
func billedReasoning(s pricing.Spec) pricing.Spec {
	out := s
	if out.Prices.Reasoning == "" {
		out.Prices.Reasoning = out.Prices.Output
	}
	out.Tiers = slices.Clone(s.Tiers)
	for i, t := range out.Tiers {
		if t.Prices.Reasoning == "" && s.Prices.Reasoning == "" && t.Prices.Output != "" {
			out.Tiers[i].Prices.Reasoning = t.Prices.Output
		}
	}
	return out
}

// Diff lists the rate changes from cur to prop, and whether the tier boundaries differ. Reasoning
// compares as billed (billedReasoning): an unset rate equals the output rate.
func Diff(cur, prop pricing.Spec) (changes []Change, boundariesDiffer bool) {
	cur, prop = billedReasoning(cur), billedReasoning(prop)
	cmp := func(over int64, a, b pricing.Wire) {
		for _, bk := range Buckets {
			from, to := wireBucket(a, bk), wireBucket(b, bk)
			if !SameRate(from, to) {
				changes = append(changes, Change{Bucket: bk, ContextOver: over, From: from, To: to})
			}
		}
	}
	cmp(0, cur.Prices, prop.Prices)
	byOver := func(s pricing.Spec) map[int64]pricing.Wire {
		m := map[int64]pricing.Wire{}
		for _, t := range s.Tiers {
			m[t.ContextOver] = t.Prices
		}
		return m
	}
	cm, pm := byOver(cur), byOver(prop)
	for _, t := range prop.Tiers {
		if c, ok := cm[t.ContextOver]; ok {
			cmp(t.ContextOver, c, t.Prices)
		}
	}
	for over := range cm {
		if _, ok := pm[over]; !ok {
			boundariesDiffer = true
		}
	}
	for over := range pm {
		if _, ok := cm[over]; !ok {
			boundariesDiffer = true
		}
	}
	return changes, boundariesDiffer
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// Differs reports whether prop prices differently from cur: a rate, a bucket or a tier boundary.
func Differs(cur, prop pricing.Spec) bool {
	changes, boundaries := Diff(cur, prop)
	return len(changes) > 0 || boundaries
}

// EntryID is the platform entry id of a model of a provider type, in the scheme of the default
// list ("openrouter-deepseek-v4-1-flash"), so that a release shipping the same price retires the
// platform entry (ADR 0098 decision 1.5).
func EntryID(providerType, model string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(providerType+"-"+model), "-"), "-")
}

// SourceURL is where a source's prices are read, for the entry's source field.
func SourceURL(source string) string {
	if source == SourceOpenRouter {
		return OpenRouterURL
	}
	return ""
}

// BuildEntry makes the entry an offer proposes, from the entry that prices the model today (cur,
// nil for none). Rates and tiers come from the offer. Tokenizer, ratio, tool overhead and hidden
// thinking are kept from cur (a new model gets the bytes tokenizer at 3.0 bytes per token). The
// max output is kept from cur when cur is the model's own entry, otherwise the source's. An entry
// that does not validate is an error: the proposal is refused.
func BuildEntry(o Offer, cur *pricing.Entry, source string, now time.Time) (pricing.Entry, error) {
	if len(o.Problems) > 0 {
		return pricing.Entry{}, errors.New(strings.Join(o.Problems, "; "))
	}
	e := pricing.Entry{
		ID: EntryID(o.ProviderType, o.Model), Provider: o.ProviderType, Match: pricing.Match{Glob: o.Model},
		ValidFrom: now.UTC().Format(time.DateOnly), Spec: o.Spec, Tokenizer: pricing.TokenizerBytes, Ratio: "3.0",
		MaxOutput: o.MaxOutput,
		Source:    pricing.Source{URL: SourceURL(source), Accessed: now.UTC().Format(time.DateOnly)},
		Note:      "Platform price from " + source + "; the provenance of the platform price version has the fetch time and body hash.",
	}
	if cur != nil {
		e.Tokenizer, e.Ratio, e.ToolOverheadTokens, e.HiddenThinking = cur.Tokenizer, cur.Ratio, cur.ToolOverheadTokens, cur.HiddenThinking
		if cur.Provider == o.ProviderType && cur.Match.Glob == o.Model && cur.Match.Regex == "" {
			e.ID = cur.ID
			if cur.MaxOutput > 0 {
				e.MaxOutput = cur.MaxOutput
			}
		} else if e.MaxOutput == 0 {
			e.MaxOutput = cur.MaxOutput
		}
	}
	if e.MaxOutput == 0 {
		// The source gives no output limit (OpenRouter's max_completion_tokens is null) and no
		// current entry has one. The rest of the entry must still be valid; a person then supplies
		// the limit when accepting (prices proposals accept --max-output).
		probe := e
		probe.MaxOutput = 1
		if _, err := probe.Resolve(true); err != nil {
			return pricing.Entry{}, err
		}
		return e, ErrNoMaxOutput
	}
	if _, err := e.Resolve(true); err != nil {
		return pricing.Entry{}, err
	}
	return e, nil
}

// ErrNoMaxOutput is BuildEntry's error for an otherwise valid entry that has no max_output: the
// source reports none and no current entry has one. The proposal waits for a person, who sets it.
var ErrNoMaxOutput = errors.New("max_output must be set: the source reports no output limit for this model (accept with --max-output <tokens>)")

// Facts are everything Classify needs.
type Facts struct {
	// Origin is "fetch" or "import". An import is never auto.
	Origin string
	Source string
	// Current is the entry that prices the model today (platform, else default); nil for a model
	// with no price.
	Current *pricing.Entry
	// Baseline is the price spec a person last reviewed (the default entry or the last platform
	// version a person wrote). Nil means Current's.
	Baseline *pricing.Spec
	Proposed pricing.Entry
	// Agreeing are the sources that returned this model's proposed values, this one included.
	Agreeing []string
	// Stable: a live source returned the values unchanged across two fetches at least StabilityWindow apart.
	Stable bool
}

// Verdict is the classification of one proposal.
type Verdict struct {
	Class   Class    `json:"class"`
	Changes []Change `json:"changes"`
	// Failed lists the rules that keep the proposal from auto, each with its numbers. A confirm
	// proposal also lists its confirmation reasons here.
	Failed []string `json:"failed,omitempty"`
}

// primarySources names, per provider type, the source that is the provider's own list.
var primarySources = map[string]string{"openrouter": SourceOpenRouter}

// independent says whether a source counts as a separate maintainer and data path for a provider
// type. models.dev never counts towards auto-apply (ADR 0098 decision 7).
func independent(_, source string) bool { return source != "models-dev" }

// Agrees applies the agreement rule: the model's primary source, where one exists, or else at
// least two independent sources with the same normalized value.
func Agrees(providerType, source string, agreeing []string) bool {
	if p, ok := primarySources[providerType]; ok {
		return source == p
	}
	n := 0
	seen := map[string]bool{}
	for _, s := range agreeing {
		if !seen[s] && independent(providerType, s) {
			seen[s] = true
			n++
		}
	}
	return n >= 2
}

func rateOf(s string) (money.Rate, bool) {
	if s == "" {
		return 0, false
	}
	r, err := money.ParseRate(s)
	return r, err == nil
}

// Classify applies decision 3 to a proposal. Structural refusals happen earlier, in BuildEntry; a
// proposal that gets here has a valid entry.
func Classify(f Facts, b Bounds) Verdict {
	v := Verdict{Class: ClassAuto}
	var confirm, review []string
	baseline := f.Proposed.Spec
	if f.Current != nil {
		baseline = f.Current.Spec
		if f.Baseline != nil {
			baseline = *f.Baseline
		}
		var boundaries bool
		v.Changes, boundaries = Diff(f.Current.Spec, f.Proposed.Spec)
		if boundaries {
			review = append(review, "the tier boundaries differ from the current price")
		}
	} else {
		// A model with no Koinon price has no baseline: every rate is added, the change bound does
		// not apply, and the checks below that need no baseline (high rate, zero rate, unit mix-up
		// within the entry, agreement, stability) decide (ADR 0098, amendment of 2026-11-07).
		confirm, review = newModelChecks(f.Proposed.Spec)
		for _, bk := range Buckets {
			if to := wireBucket(f.Proposed.Prices, bk); to != "" {
				v.Changes = append(v.Changes, Change{Bucket: bk, To: to})
			}
		}
		for _, t := range f.Proposed.Tiers {
			for _, bk := range Buckets {
				if to := wireBucket(t.Prices, bk); to != "" {
					v.Changes = append(v.Changes, Change{Bucket: bk, ContextOver: t.ContextOver, To: to})
				}
			}
		}
	}
	if f.Current != nil {
		if f.Current.Provider != f.Proposed.Provider {
			review = append(review, fmt.Sprintf("the provider type differs (%s against %s)", f.Current.Provider, f.Proposed.Provider))
		}
		if f.Current.ID != f.Proposed.ID {
			review = append(review, fmt.Sprintf("the proposal is a new entry %s: the current price is entry %s, which also covers other models", f.Proposed.ID, f.Current.ID))
		}
	}
	for _, c := range v.Changes {
		where := c.Bucket
		if c.ContextOver > 0 {
			where = fmt.Sprintf("%s above %d tokens", c.Bucket, c.ContextOver)
		}
		switch {
		case c.From == "" && f.Current != nil:
			review = append(review, fmt.Sprintf("%s is a new priced bucket (%s)", where, c.To))
		case c.To == "" && f.Current != nil:
			review = append(review, fmt.Sprintf("%s is no longer priced by the source (it was %s)", where, c.From))
		}
		from, okFrom := rateOf(c.From)
		to, okTo := rateOf(c.To)
		if okTo && to > mustRate(fmt.Sprint(HighRateUSDPerMillion)) {
			confirm = append(confirm, fmt.Sprintf("%s is %s USD per million tokens, above %d", where, c.To, HighRateUSDPerMillion))
		}
		if okFrom && okTo && f.Current != nil {
			switch {
			case from > 0 && to == 0:
				confirm = append(confirm, fmt.Sprintf("%s goes to zero from %s", where, c.From))
				review = append(review, fmt.Sprintf("%s goes to zero from %s: no rate may go to zero", where, c.From))
			case from > 0:
				if to >= from*UnitMixupFactor || to*UnitMixupFactor <= from {
					confirm = append(confirm, fmt.Sprintf("%s moves from %s to %s (a ratio of %s): suspected unit mix-up, per token against per million is 10^6 and per thousand 10^3", where, c.From, c.To, ratio(to, from)))
				} else if to > from*ConfirmFactor || to*ConfirmFactor < from {
					confirm = append(confirm, fmt.Sprintf("%s moves from %s to %s, %s of the current rate", where, c.From, c.To, ratio(to, from)))
				}
			}
		}
	}
	// A move is bounded against the last person-reviewed rate, so small moves cannot add up.
	if f.Current != nil {
		review = append(review, overBound(baseline, f.Proposed.Spec, b.MaxChangePercent)...)
	}
	switch {
	case f.Origin != "fetch":
		review = append(review, "an uploaded file is never applied by itself: a person reviews it")
	case !Agrees(f.Proposed.Provider, f.Source, f.Agreeing):
		review = append(review, fmt.Sprintf("agreement: %s is not the provider's own list and fewer than two independent sources agree (%s)", f.Source, strings.Join(f.Agreeing, ", ")))
	}
	if f.Origin == "fetch" && !f.Stable {
		review = append(review, fmt.Sprintf("pinning: %s has not returned these values in two fetches at least %s apart yet", f.Source, StabilityWindow))
	}
	v.Failed = append(confirm, review...)
	switch {
	case len(confirm) > 0:
		v.Class = ClassConfirm
	case len(review) > 0:
		v.Class = ClassNeedsReview
	}
	return v
}

func mustRate(s string) money.Rate {
	r, err := money.ParseRate(s)
	if err != nil {
		panic(err)
	}
	return r
}

func ratio(to, from money.Rate) string {
	r := new(big.Rat).SetFrac64(int64(to), int64(from))
	f, _ := r.Float64()
	return fmt.Sprintf("%.3gx", f)
}

// newModelChecks are the rules for a model with no current price that stand in for the ones that
// compare against it. A zero rate waits for a person (no rate is ever set to zero by itself). A
// suspected unit mix-up is measured inside the entry: a base rate UnitMixupFactor or more away
// from the input rate, or a tier rate that far from the base rate of its bucket, needs a typed
// confirmation (per token against per million is 10^6, per thousand 10^3).
func newModelChecks(s pricing.Spec) (confirm, review []string) {
	mixup := func(where, a, b string, ra, rb money.Rate) {
		if ra > 0 && rb > 0 && (rb >= ra*UnitMixupFactor || rb*UnitMixupFactor <= ra) {
			confirm = append(confirm, fmt.Sprintf("%s: %s against %s (a ratio of %s): suspected unit mix-up, per token against per million is 10^6 and per thousand 10^3", where, b, a, ratio(rb, ra)))
		}
	}
	zero := func(where, val string) {
		if r, ok := rateOf(val); ok && r == 0 {
			review = append(review, fmt.Sprintf("%s is zero: a new model's zero rate is never applied by itself", where))
		}
	}
	in, inOK := rateOf(s.Prices.Input)
	for _, bk := range Buckets {
		v := wireBucket(s.Prices, bk)
		zero(bk, v)
		if r, ok := rateOf(v); ok && inOK && bk != "input" {
			mixup(bk+" against input", s.Prices.Input, v, in, r)
		}
	}
	for _, t := range s.Tiers {
		for _, bk := range Buckets {
			v := wireBucket(t.Prices, bk)
			where := fmt.Sprintf("%s above %d tokens", bk, t.ContextOver)
			zero(where, v)
			base := wireBucket(s.Prices, bk)
			rb, okB := rateOf(base)
			if r, ok := rateOf(v); ok && okB {
				mixup(where+" against the base rate", base, v, rb, r)
			}
		}
	}
	return confirm, review
}

// overBound lists the base and tier rates of prop that moved more than pct percent from base.
func overBound(base, prop pricing.Spec, pct int) []string {
	base, prop = billedReasoning(base), billedReasoning(prop)
	var out []string
	check := func(where string, a, b pricing.Wire) {
		for _, bk := range Buckets {
			from, okFrom := rateOf(wireBucket(a, bk))
			to, okTo := rateOf(wireBucket(b, bk))
			if !okFrom || !okTo || from == to {
				continue
			}
			diff := int64(to) - int64(from)
			if diff < 0 {
				diff = -diff
			}
			if from == 0 || diff*100 > int64(from)*int64(pct) {
				out = append(out, fmt.Sprintf("%s%s moves from the last reviewed %s to %s, more than %d%%", bk, where, wireBucket(a, bk), wireBucket(b, bk), pct))
			}
		}
	}
	check("", base.Prices, prop.Prices)
	for _, t := range prop.Tiers {
		if i := slices.IndexFunc(base.Tiers, func(x pricing.WireTier) bool { return x.ContextOver == t.ContextOver }); i >= 0 {
			check(fmt.Sprintf(" above %d tokens", t.ContextOver), base.Tiers[i].Prices, t.Prices)
		}
	}
	return out
}

// CapAllows applies the daily cap: when appliedToday plus this run's auto candidates would exceed
// maxPerDay, none of them applies (a large batch looks like a broken source).
func CapAllows(appliedToday, candidates, maxPerDay int) bool {
	return appliedToday+candidates <= maxPerDay
}
