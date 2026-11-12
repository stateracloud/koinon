// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// Helpers shared by the aggregator adapters (LiteLLM, genai-prices, models.dev). They read JSON
// numbers from their decimal text, never through float64 (ADR 0098 decision 2.5): a json.RawMessage
// keeps the text the source wrote, and big.Rat parses it exactly.

// perMillionDirect checks a USD-per-million-tokens decimal as the source wrote it and returns it
// normalized to at most six fraction digits, with no rounding: a value with more digits is refused.
func perMillionDirect(s string) (string, error) {
	if !decimalRE.MatchString(s) {
		return "", fmt.Errorf("%q is not a non-negative decimal", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("%q is not a number", s)
	}
	scaled := new(big.Rat).Mul(r, big.NewRat(1_000_000, 1))
	if !scaled.IsInt() {
		return "", fmt.Errorf("%q has more than six fraction digits", s)
	}
	whole, frac := new(big.Int).QuoRem(scaled.Num(), big.NewInt(1_000_000), new(big.Int))
	out := whole.String()
	if f := strings.TrimRight(fmt.Sprintf("%06d", frac.Int64()), "0"); f != "" {
		out += "." + f
	}
	if _, err := money.ParseRate(out); err != nil {
		return "", err
	}
	return out, nil
}

// numberText returns the text of a JSON number, "" when the value is absent or null. Anything else
// (a string, an object) is a problem: the source's format is not the one this adapter reads.
func numberText(raw json.RawMessage) (text string, present bool, err error) {
	t := strings.TrimSpace(string(raw))
	switch {
	case t == "" || t == "null":
		return "", false, nil
	case t[0] == '"' || t[0] == '{' || t[0] == '[' || t == "true" || t == "false":
		return "", true, fmt.Errorf("%s is not a number", truncateText(t))
	}
	return t, true, nil
}

func truncateText(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// bucketValue reads one price (a JSON number) with conv, which turns its text into USD per
// million. A negative or unreadable one is a problem naming where.
func bucketValue(raw json.RawMessage, where string, conv func(string) (string, error), problems *[]string) string {
	t, present, err := numberText(raw)
	if err != nil {
		*problems = append(*problems, where+": "+err.Error())
		return ""
	}
	if !present {
		return ""
	}
	if strings.HasPrefix(t, "-") {
		*problems = append(*problems, fmt.Sprintf("%s: negative price %s (a variable price Koinon cannot bill)", where, t))
		return ""
	}
	v, err := conv(t)
	if err != nil {
		*problems = append(*problems, where+": "+err.Error())
		return ""
	}
	return v
}

// tierSet collects the rates a source sets above context thresholds.
type tierSet map[int64]*pricing.Wire

func (t tierSet) at(over int64) *pricing.Wire {
	w, ok := t[over]
	if !ok {
		w = &pricing.Wire{}
		t[over] = w
	}
	return w
}

// tiers turns the collected rates into Koinon tiers in ascending order. A tier reasoning rate goes
// through separateReasoning against the tier's own output rate, else the base one; a tier that
// prices exactly like the one below it is dropped (as mergeTiers does).
func (t tierSet) tiers(base pricing.Wire) []pricing.WireTier {
	overs := make([]int64, 0, len(t))
	for o := range t {
		overs = append(overs, o)
	}
	sort.Slice(overs, func(i, j int) bool { return overs[i] < overs[j] })
	var out []pricing.WireTier
	prev := base
	for _, o := range overs {
		w := *t[o]
		if w.Reasoning != "" {
			outRate := w.Output
			if outRate == "" {
				outRate = base.Output
			}
			w.Reasoning = separateReasoning(w.Reasoning, outRate)
			if w.Reasoning == "" && prev.Reasoning != "" {
				w.Reasoning = outRate
			}
		}
		if w == (pricing.Wire{}) || effectiveWire(prev, w) == prev {
			continue
		}
		out = append(out, pricing.WireTier{ContextOver: o, Prices: w})
		prev = effectiveWire(prev, w)
	}
	return out
}

// maxOutputOf reads an output limit in tokens; 0 when absent. Anything else is a problem.
func maxOutputOf(raw json.RawMessage, where string, problems *[]string) int {
	t, present, err := numberText(raw)
	if err != nil || !present {
		if err != nil {
			*problems = append(*problems, where+": "+err.Error())
		}
		return 0
	}
	r, ok := new(big.Rat).SetString(t)
	if !ok || !r.IsInt() || r.Sign() < 0 || r.Num().Cmp(big.NewInt(10_000_000)) > 0 {
		*problems = append(*problems, where+" is not a count")
		return 0
	}
	return int(r.Num().Int64())
}

// decodeStrict decodes one JSON value, refusing trailing data.
func decodeStrict(source string, body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("pricefetch: %s: decode: %w", source, err)
	}
	if d.More() {
		return fmt.Errorf("pricefetch: %s: trailing data after the body", source)
	}
	return nil
}

// finishOffer applies the checks every adapter shares to a normalized offer.
func finishOffer(o *Offer) {
	if o.Spec.Prices.Input == "" || o.Spec.Prices.Output == "" {
		o.Problems = append(o.Problems, "input and output prices are required")
	}
	if len(o.Ignored) == 0 {
		o.Ignored = nil
	}
}

// addOffer appends o to snap, merging a model listed twice: equal prices are one offer, different
// ones make the offer unusable (a person decides), never an arbitrary pick.
func addOffer(snap *Snapshot, index map[string]int, o Offer) {
	key := o.ProviderType + "\x00" + o.Model
	if i, ok := index[key]; ok {
		if SpecHash(snap.Offers[i].Spec) != SpecHash(o.Spec) {
			snap.Offers[i].Problems = append(snap.Offers[i].Problems, "the model is listed twice with different prices")
		}
		return
	}
	index[key] = len(snap.Offers)
	snap.Offers = append(snap.Offers, o)
}

func sortOffers(snap *Snapshot) {
	sort.Slice(snap.Offers, func(i, j int) bool { return snap.Offers[i].Model < snap.Offers[j].Model })
}

var errNotObject = errors.New("is not an object")
