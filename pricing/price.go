// SPDX-License-Identifier: Apache-2.0

// Package pricing is Koinon's price schema and default price list (plan item PV2, ADR 0056).
//
// A price is a rate per usage type, in USD per million tokens, for non-overlapping buckets of
// one request: input (not cached), cache read, cache write, output (not reasoning) and
// reasoning. A token is never in two buckets, so it is never billed twice. Optional tiers
// replace rates once the input side of a request (input, cache reads and cache writes
// together) exceeds a number of tokens, as in agentgateway's model catalog. Rates are exact
// integers (package money); no floats.
//
// An [Entry] adds a match pattern (glob or regex on the model name, for a provider type), an
// effective date, the tokenizer and byte ratio used to estimate missing usage, and a source.
// The default list shipped in the binary is default.yaml; org admins add their own entries,
// which win.
package pricing

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/money"
)

// Wire is a set of rates as written in YAML, JSON and the admin API: decimal strings in USD
// per million tokens with at most six fraction digits. An empty field is unset; a free model
// needs an explicit "0".
type Wire struct {
	Input      string `json:"input,omitempty" yaml:"input,omitempty"`
	Output     string `json:"output,omitempty" yaml:"output,omitempty"`
	CacheRead  string `json:"cacheRead,omitempty" yaml:"cache_read,omitempty"`
	CacheWrite string `json:"cacheWrite,omitempty" yaml:"cache_write,omitempty"`
	// CacheWrite1h is the rate for cache writes with a 1-hour TTL; CacheWrite is the 5-minute rate.
	// Unset, 1-hour writes are billed at CacheWrite.
	CacheWrite1h string `json:"cacheWrite1h,omitempty" yaml:"cache_write_1h,omitempty"`
	Reasoning    string `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
}

// WireTier is a tier as written.
type WireTier struct {
	ContextOver int64 `json:"contextOver" yaml:"context_over"`
	Prices      Wire  `json:"prices" yaml:"prices"`
}

// Spec is prices and tiers as written.
type Spec struct {
	Prices Wire       `json:"prices" yaml:"prices"`
	Tiers  []WireTier `json:"tiers,omitempty" yaml:"tiers,omitempty"`
}

// Rates are parsed rates. Nil means unset.
type Rates struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h, Reasoning *money.Rate
}

func (r Rates) nonzero() bool {
	for _, p := range []*money.Rate{r.Input, r.Output, r.CacheRead, r.CacheWrite, r.CacheWrite1h, r.Reasoning} {
		if p != nil && *p != 0 {
			return true
		}
	}
	return false
}

// Tier is a parsed tier. Its rates overlay the base rates.
type Tier struct {
	ContextOver int64
	Rates       Rates
}

// Price is a parsed Spec.
type Price struct {
	Base  Rates
	Tiers []Tier // ascending ContextOver
}

// Usage is one request's tokens in non-overlapping buckets.
type Usage struct {
	Input      int64 // input tokens that were neither cache reads nor cache writes
	CacheRead  int64
	CacheWrite int64 // 5-minute cache writes
	// CacheWrite1h is cache writes with a 1-hour TTL, a bucket of its own so that the price
	// version and the raw usage stay reproducible (ADR 0070 amendment).
	CacheWrite1h int64
	Output       int64 // output tokens that were not reasoning
	Reasoning    int64
}

// Context is the input side of the request, which selects the tier.
func (u Usage) Context() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.CacheWrite1h }

// Total is every token.
func (u Usage) Total() int64 { return u.Context() + u.Output + u.Reasoning }

func parseRate(name, s string) (*money.Rate, error) {
	if s == "" {
		return nil, nil
	}
	r, err := money.ParseRate(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &r, nil
}

// Parse validates and parses w.
func (w Wire) Parse() (Rates, error) {
	var r Rates
	var errs []error
	for _, f := range []struct {
		name string
		s    string
		dst  **money.Rate
	}{
		{"input", w.Input, &r.Input},
		{"output", w.Output, &r.Output},
		{"cache_read", w.CacheRead, &r.CacheRead},
		{"cache_write", w.CacheWrite, &r.CacheWrite},
		{"cache_write_1h", w.CacheWrite1h, &r.CacheWrite1h},
		{"reasoning", w.Reasoning, &r.Reasoning},
	} {
		v, err := parseRate(f.name, f.s)
		if err != nil {
			errs = append(errs, err)
		}
		*f.dst = v
	}
	return r, errors.Join(errs...)
}

// Wire formats the rates.
func (r Rates) Wire() Wire {
	f := func(p *money.Rate) string {
		if p == nil {
			return ""
		}
		return p.PerMillion()
	}
	return Wire{Input: f(r.Input), Output: f(r.Output), CacheRead: f(r.CacheRead), CacheWrite: f(r.CacheWrite), CacheWrite1h: f(r.CacheWrite1h), Reasoning: f(r.Reasoning)}
}

// Parse validates s: input and output are required, tiers ascend and set at least one rate.
func (s Spec) Parse() (Price, error) {
	base, err := s.Prices.Parse()
	if err != nil {
		return Price{}, err
	}
	if base.Input == nil || base.Output == nil {
		return Price{}, errors.New("prices: input and output are required (a free model needs an explicit \"0\")")
	}
	p := Price{Base: base}
	var prev int64
	for i, t := range s.Tiers {
		if t.ContextOver <= prev {
			return Price{}, fmt.Errorf("tiers[%d]: context_over must be positive and ascending", i)
		}
		prev = t.ContextOver
		r, err := t.Prices.Parse()
		if err != nil {
			return Price{}, fmt.Errorf("tiers[%d]: %w", i, err)
		}
		if r == (Rates{}) {
			return Price{}, fmt.Errorf("tiers[%d]: sets no rate", i)
		}
		p.Tiers = append(p.Tiers, Tier{ContextOver: t.ContextOver, Rates: r})
	}
	return p, nil
}

// Effective returns the rates for a request whose input side is context tokens: the base rates
// overlaid by the tier with the largest ContextOver that context exceeds.
func (p Price) Effective(context int64) Rates {
	out := p.Base
	for _, t := range p.Tiers {
		if context > t.ContextOver {
			out = out.overlay(t.Rates)
		}
	}
	return out
}

func (r Rates) overlay(t Rates) Rates {
	pick := func(base, over *money.Rate) *money.Rate {
		if over != nil {
			return over
		}
		return base
	}
	return Rates{pick(r.Input, t.Input), pick(r.Output, t.Output), pick(r.CacheRead, t.CacheRead), pick(r.CacheWrite, t.CacheWrite), pick(r.CacheWrite1h, t.CacheWrite1h), pick(r.Reasoning, t.Reasoning)}
}

// Breakdown is the cost of each bucket.
type Breakdown struct {
	Input, CacheRead, CacheWrite, CacheWrite1h, Output, Reasoning money.USD
}

// Total adds the buckets.
func (b Breakdown) Total() (money.USD, error) {
	var sum money.USD
	var err error
	for _, v := range []money.USD{b.Input, b.CacheRead, b.CacheWrite, b.CacheWrite1h, b.Output, b.Reasoning} {
		if sum, err = sum.Add(v); err != nil {
			return 0, err
		}
	}
	return sum, nil
}

// Cost prices u. A bucket without its own rate is billed at the rate it would be if the
// provider did not split it out, as agentgateway does: cache reads and writes at the input
// rate, 1-hour cache writes at the cache-write rate, reasoning at the output rate. Nothing is billed twice because the buckets do not overlap.
func (p Price) Cost(u Usage) (Breakdown, money.USD, error) {
	r := p.Effective(u.Context())
	rate := func(own, fallback *money.Rate) money.Rate {
		if own != nil {
			return *own
		}
		return *fallback
	}
	write5m := rate(r.CacheWrite, r.Input)
	var b Breakdown
	var err error
	for _, l := range []struct {
		dst    *money.USD
		tokens int64
		rate   money.Rate
	}{
		{&b.Input, u.Input, *r.Input},
		{&b.CacheRead, u.CacheRead, rate(r.CacheRead, r.Input)},
		{&b.CacheWrite, u.CacheWrite, rate(r.CacheWrite, r.Input)},
		{&b.CacheWrite1h, u.CacheWrite1h, rate(r.CacheWrite1h, &write5m)},
		{&b.Output, u.Output, *r.Output},
		{&b.Reasoning, u.Reasoning, rate(r.Reasoning, r.Output)},
	} {
		if *l.dst, err = l.rate.Cost(l.tokens); err != nil {
			return Breakdown{}, 0, err
		}
	}
	total, err := b.Total()
	return b, total, err
}

// Gateway converts p for agentgateway's model catalog (the gateway's own budget backstop).
func (p Price) Gateway() gateway.Price {
	w := p.Base.Wire()
	g := gateway.Price{Input: w.Input, Output: w.Output, CacheRead: w.CacheRead, CacheWrite: w.CacheWrite, CacheWrite1h: w.CacheWrite1h, Reasoning: w.Reasoning}
	for _, t := range p.Tiers {
		tw := t.Rates.Wire()
		g.Tiers = append(g.Tiers, gateway.PriceTier{
			ContextOver: t.ContextOver, Input: tw.Input, Output: tw.Output, CacheRead: tw.CacheRead,
			CacheWrite: tw.CacheWrite, CacheWrite1h: tw.CacheWrite1h, Reasoning: tw.Reasoning,
		})
	}
	return g
}

// Match selects model names. Exactly one field is set.
type Match struct {
	// Glob uses * (any characters, including /) and ? (one character), case-insensitively,
	// against the whole name.
	Glob string `json:"glob,omitempty" yaml:"glob,omitempty"`
	// Regex is an RE2 expression matched case-insensitively against the whole name.
	Regex string `json:"regex,omitempty" yaml:"regex,omitempty"`
}

// Compile returns the matcher and a specificity: the number of literal characters, so a
// narrower pattern beats a wider one.
func (m Match) Compile() (re *regexp.Regexp, specificity int, err error) {
	switch {
	case (m.Glob == "") == (m.Regex == ""):
		return nil, 0, errors.New("match: set exactly one of glob and regex")
	case m.Glob != "":
		var b strings.Builder
		b.WriteString("(?is)^")
		for _, c := range m.Glob {
			switch c {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
				specificity++
			default:
				b.WriteString(regexp.QuoteMeta(string(c)))
				specificity++
			}
		}
		b.WriteString("$")
		re, err = regexp.Compile(b.String())
		return re, specificity, err
	default:
		if len(m.Regex) > 1024 {
			return nil, 0, errors.New("match: regex longer than 1024 characters")
		}
		re, err = regexp.Compile("(?i)^(?:" + m.Regex + ")$")
		// Inline flag groups, (?flags:...) and (?flags), are syntax, not literal characters.
		lit := inlineFlagsRE.ReplaceAllString(m.Regex, "")
		return re, len(regexp.MustCompile(`[^a-zA-Z0-9_./:@-]`).ReplaceAllString(lit, "")), err
	}
}

var inlineFlagsRE = regexp.MustCompile(`\(\?[a-zA-Z]*(-[a-zA-Z]+)?[:)]`)

// Tokenizers.
const (
	TokenizerBytes      = "bytes"
	TokenizerO200K      = "tiktoken:o200k_base"  //nolint:gosec // encoding name, not a credential
	TokenizerCL100K     = "tiktoken:cl100k_base" //nolint:gosec // encoding name, not a credential
	maxOverheadTokens   = 100000
	maxOutputTokens     = 10_000_000
	providerWildcard    = "*"
	idPattern           = `^[a-z0-9][a-z0-9._-]{0,127}$`
	providerTypePattern = `^(\*|[a-z][a-z0-9-]{0,62})$`
)

var (
	idRE       = regexp.MustCompile(idPattern)
	providerRE = regexp.MustCompile(providerTypePattern)
)

// Source says where a price comes from.
type Source struct {
	URL      string `json:"url" yaml:"url"`
	Accessed string `json:"accessed" yaml:"accessed"` // yyyy-mm-dd
}

// Entry is one dated price for the models a pattern matches.
type Entry struct {
	ID string `json:"id" yaml:"id"`
	// Provider is the provider type the price applies to (openai, openrouter, ollama, ...);
	// "*" means any.
	Provider  string `json:"provider" yaml:"provider"`
	Match     Match  `json:"match" yaml:"match"`
	ValidFrom string `json:"validFrom" yaml:"valid_from"` // yyyy-mm-dd, UTC
	Spec      `yaml:",inline"`
	// Tokenizer and Ratio estimate usage a provider did not report (package estimate). Ratio
	// is UTF-8 bytes per token and is also the fallback when the tokenizer cannot be used.
	Tokenizer string `json:"tokenizer" yaml:"tokenizer"`
	Ratio     string `json:"ratio" yaml:"ratio"`
	// ToolOverheadTokens is the fixed input a provider adds when a request defines tools.
	ToolOverheadTokens int `json:"toolOverheadTokens,omitempty" yaml:"tool_overhead_tokens,omitempty"`
	// HiddenThinking marks models that bill thinking tokens the response may not show, so an
	// aborted request cannot be measured from its text (estimate rule 3).
	HiddenThinking bool `json:"hiddenThinking,omitempty" yaml:"hidden_thinking,omitempty"`
	// MaxOutput is the model's largest output in tokens. It caps the max_tokens bound a response
	// cut short holds back from the gateway key, and stands in when the request's limit was not
	// logged (for example OpenAI's max_completion_tokens).
	MaxOutput int    `json:"maxOutput,omitempty" yaml:"max_output,omitempty"`
	Source    Source `json:"source" yaml:"source"`
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
}

// Resolved is an Entry parsed.
type Resolved struct {
	Entry       Entry
	Price       Price
	Matcher     *regexp.Regexp
	Specificity int
	ValidFrom   time.Time
	RatioValue  float64
}

// Resolve validates e. orgEntry relaxes the source requirement (an org's own price needs none).
func (e Entry) Resolve(orgEntry bool) (Resolved, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !idRE.MatchString(e.ID) {
		bad("id %q must match %s", e.ID, idPattern)
	}
	if !providerRE.MatchString(e.Provider) {
		bad("provider %q must be a provider type or *", e.Provider)
	}
	re, spec, err := e.Match.Compile()
	if err != nil {
		bad("%v", err)
	}
	from, err := time.Parse(time.DateOnly, e.ValidFrom)
	if err != nil {
		bad("valid_from %q must be a date yyyy-mm-dd", e.ValidFrom)
	}
	price, err := e.Parse()
	if err != nil {
		bad("%v", err)
	}
	if !slices.Contains([]string{TokenizerBytes, TokenizerO200K, TokenizerCL100K}, e.Tokenizer) {
		bad("tokenizer %q must be bytes, tiktoken:o200k_base or tiktoken:cl100k_base", e.Tokenizer)
	}
	ratio, err := ParseRatio(e.Ratio)
	if err != nil {
		bad("ratio: %v", err)
	}
	if e.ToolOverheadTokens < 0 || e.ToolOverheadTokens > maxOverheadTokens {
		bad("tool_overhead_tokens out of range")
	}
	if e.MaxOutput < 0 || e.MaxOutput > maxOutputTokens {
		bad("max_output out of range")
	}
	if e.MaxOutput == 0 && price.Base.nonzero() {
		bad("max_output is required for a priced entry: an aborted response settles at no more than it")
	}
	if !orgEntry {
		if !strings.HasPrefix(e.Source.URL, "https://") {
			bad("source.url must be an https URL of the provider's pricing page")
		}
		if d, err := time.Parse(time.DateOnly, e.Source.Accessed); err != nil || d.After(time.Now().Add(24*time.Hour)) {
			bad("source.accessed must be a date yyyy-mm-dd that is not in the future")
		}
	}
	if len(errs) > 0 {
		return Resolved{}, fmt.Errorf("price entry %q: %w", e.ID, errors.Join(errs...))
	}
	return Resolved{Entry: e, Price: price, Matcher: re, Specificity: spec, ValidFrom: from, RatioValue: ratio}, nil
}

// ParseRatio reads bytes per token: a decimal between 0.5 and 16.
func ParseRatio(s string) (float64, error) {
	if !regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,3})?$`).MatchString(s) {
		return 0, fmt.Errorf("%q is not a decimal such as 3.0", s)
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil || v < 0.5 || v > 16 {
		return 0, fmt.Errorf("%q must be between 0.5 and 16", s)
	}
	return v, nil
}

// Select returns the entry of rs that prices model on a provider of type provider at time at:
// among entries whose pattern matches and whose valid_from is not after at, an entry for the
// exact provider beats "*", then the more specific pattern, then the later valid_from, then
// the earlier position in rs.
func Select(rs []Resolved, provider, model string, at time.Time) (Resolved, bool) {
	best, found := -1, false
	for i, r := range rs {
		if r.ValidFrom.After(at) || !r.Matcher.MatchString(model) {
			continue
		}
		if r.Entry.Provider != providerWildcard && r.Entry.Provider != provider {
			continue
		}
		if !found || better(r, rs[best], provider) {
			best, found = i, true
		}
	}
	if !found {
		return Resolved{}, false
	}
	return rs[best], true
}

func better(a, b Resolved, provider string) bool {
	if ae, be := a.Entry.Provider == provider, b.Entry.Provider == provider; ae != be {
		return ae
	}
	if a.Specificity != b.Specificity {
		return a.Specificity > b.Specificity
	}
	return a.ValidFrom.After(b.ValidFrom)
}
