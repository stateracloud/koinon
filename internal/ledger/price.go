// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/estimate"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providertype"
	"github.com/stateracloud/koinon/pricing"
)

// FlagThreshold is the relative difference between Koinon's cost and the gateway's above which
// the price entry is flagged (1%).
var FlagThreshold = big.NewRat(1, 100)

// priced is what the ledger records for one request.
type priced struct {
	// The token buckets are disjoint, as in the usage record (ADR 0097 decision 3): input excludes
	// the cache parts, output excludes reasoning. The spend entry stores the provider's totals
	// (migration 00013), [priced.totalIn] and [priced.totalOut].
	input, output              int64
	cacheRead, cacheWrite      int64
	reasoning                  int64
	reasoningKnown             bool
	cost                       money.USD
	gatewayCost                pgtype.Text
	ref                        pgtype.Text
	priced, estimated, flagged bool
	estimate                   []byte
	reqBytes, respBytes        pgtype.Int8
	// aborted is a response cut short: charged the usage the gateway logged. reserve is what the
	// request's max_tokens bound costs on top of that, held back from the gateway key while the
	// provider's figure is pending (ADR 0056, amendment 2026-10-28).
	aborted bool
	reserve money.USD
	// model is the served model, when Koinon knows it (its provider settles the entry).
	model prices.Model
	// waived: the provider refused the request and does not bill that; nothing is charged.
	waived bool
}

// totalIn is the input including cache reads and writes, as spend_entries.input_tokens stores it.
func (p priced) totalIn() int64 { return p.input + p.cacheRead + p.cacheWrite }

// totalOut is the output including reasoning, as spend_entries.output_tokens stores it.
func (p priced) totalOut() int64 { return p.output + p.reasoning }

// usage is the priced buckets with the output replaced by out (the reserve bound prices a larger
// output than the one charged).
func (p priced) usage(output int64) pricing.Usage {
	return pricing.Usage{Input: p.input, CacheRead: p.cacheRead, CacheWrite: p.cacheWrite, Output: output, Reasoning: p.reasoning}
}

// rejectedByUpstream: the provider answered with a 4xx and reported no usage, so it refused the
// request (invalid, flagged, not authorised, rate limited).
func rejectedByUpstream(e Entry) bool {
	return !e.InputKnown && !e.OutputKnown && e.Status >= 400 && e.Status < 500
}

// failedByUpstream: the provider answered with a 5xx and reported no usage. It generated nothing, so
// the request is charged its input estimate only, with no output and no max_tokens reserve, and still
// counts as a model request (ADR 0056, amendment 2026-10-31, old issue 340).
func failedByUpstream(e Entry) bool {
	return !e.InputKnown && !e.OutputKnown && e.Status >= 500 && e.Status < 600
}

// waivesRejected reports whether a request the provider refused is charged nothing: only when the
// model's provider type documents that it does not bill refused requests, an explicit per-type
// catalog flag (package providertype, old issue 241). A request whose provider Koinon cannot tell (no
// priced model, a custom or unknown type) keeps the conservative charge, whatever the status.
func waivesRejected(m prices.Model) bool {
	return m.ProviderType != "" && !providertype.For(m.ProviderType).BillsRejectedRequests
}

// price computes the request's cost from its usage (estimated when missing) and the price valid
// at its time, and compares it with the gateway's.
func (in *Ingester) price(ctx context.Context, q *store.Queries, org pgtype.UUID, e Entry) (priced, error) {
	p := priced{
		input: e.InputTokens, output: e.OutputTokens,
		cacheRead: e.CacheReadTokens, cacheWrite: e.CacheWriteTokens, reasoning: e.ReasoningTokens,
		reasoningKnown: e.ReasoningKnown,
	}
	if e.RequestBytes >= 0 {
		p.reqBytes = pgtype.Int8{Int64: e.RequestBytes, Valid: true}
	}
	if e.ResponseBytes >= 0 {
		p.respBytes = pgtype.Int8{Int64: e.ResponseBytes, Valid: true}
	}
	var gw money.USD
	if e.CostUSD != "" {
		g, err := gatewayCost(e.CostUSD)
		if err != nil {
			// A cost that does not fit is not a reason to lose the request.
			in.log().Warn("spend ledger: unusable gateway cost", "request", e.RequestID, "err", err)
		} else {
			gw = g
			p.gatewayCost = pgtype.Text{String: g.Numeric(), Valid: true}
		}
	}

	qt, err := in.Prices.Find(ctx, q, org, e.Model, e.Time)
	p.model = qt.Model
	if err != nil {
		err = withRequest(e.RequestID, err)
	} else {
		var out priced
		out, err = in.priceWith(ctx, q, org, e, p, qt, gw)
		if err == nil {
			return out, nil
		}
	}
	reason, ok := prices.Unpriceable(err)
	if !ok {
		return p, err
	}
	if reason == prices.ReasonUnusablePrice {
		// A stored price that cannot be used (or overflows) must not stop the batch or return
		// 503: record the request unpriced and flagged with the error. The gateway's cost, if
		// any, stands in.
		in.log().Error("spend ledger: request not priced", "agent", e.Agent.String(), "model", e.Model, "request", e.RequestID, "err", err)
		p.flagged, p.priced, p.ref = true, p.gatewayCost.Valid, pgtype.Text{}
		if p.gatewayCost.Valid {
			p.cost = gw
		}
		p.estimate, _ = json.Marshal(map[string]any{"price_error": err.Error()})
		return p, nil
	}
	// No Koinon price: the gateway's cost stands in, flagged by having no price version.
	in.log().Warn("spend ledger: request for a model without a price", "agent", e.Agent.String(), "model", e.Model, "request", e.RequestID)
	if e.CostUSD != "" {
		p.cost, p.priced = gw, true
	}
	return p, nil
}

func (in *Ingester) priceWith(ctx context.Context, q *store.Queries, org pgtype.UUID, e Entry, p priced, qt prices.Quote, gw money.USD) (priced, error) {
	res := qt.Resolution
	p.ref = pgtype.Text{String: res.Ref, Valid: true}
	p.priced = true
	meta := map[string]any{"price_ref": res.Ref}
	if res.Fallback {
		meta["price_fallback"] = "no price was valid at the request time; the current one was used"
	}
	if !e.InputKnown || !e.OutputKnown {
		params, err := prices.Calibrated(ctx, q, org, e.Model, res)
		if err != nil {
			return p, err
		}
		// Unknown output is the cut signal of a completions stream; a logged proxy error covers a
		// Messages stream, whose message_start already logged output.
		x := estimate.Input{Aborted: !e.OutputKnown || e.Error != "", MaxTokens: e.MaxTokens, Rejected: rejectedByUpstream(e), Failed: failedByUpstream(e)}
		if e.RequestBytes > 0 {
			x.RequestBytes = e.RequestBytes
		}
		switch {
		case e.ResponseBytes >= 0:
			// Measured: a 0 then says nothing was streamed (ADR 0056, amendment 2026-10-31, point 1).
			x.ResponseBytes, x.ResponseKnown = e.ResponseBytes, true
		case e.Status == 0 && e.Stream && !e.OutputKnown:
			// No status on a stream: the client left before the response headers, which a stream
			// sends before its first token, so nothing was streamed either. A response not known to
			// stream sends its headers only when the provider has generated it, so a status of 0
			// there says nothing about the output: it is estimated as an unknown size.
			x.ResponseKnown = true
		}
		if e.InputKnown {
			inTotal := p.totalIn()
			x.ReportedInput = &inTotal
		}
		if e.OutputKnown {
			outTotal := p.totalOut()
			x.ReportedOutput = &outTotal
		}
		r := estimate.Estimate(params, x)
		switch {
		case x.Rejected && waivesRejected(p.model):
			// The provider refused the request and is known not to bill that (ADR 0056, amendment
			// 2026-10-29): nothing is charged and no tokens count, but the entry stays, flagged
			// rejected_by_upstream, with the input the estimate would have charged. It is not a
			// price problem, so the price report does not flag it.
			meta["rejected_by_upstream"] = true
			meta["charge_waived"] = "provider type " + p.model.ProviderType + " does not bill refused requests"
			meta["waived_input_tokens"] = r.InputTokens
			p.waived = true
			r.InputTokens, r.OutputTokens = 0, 0
		case !e.OutputKnown:
			// A response without usage: say why it was estimated, and flag it.
			meta["usage_missing"] = true
			meta["rejected_by_upstream"] = x.Rejected
			if x.Failed {
				meta["failed_by_upstream"] = true
			}
			p.flagged = true
		}
		// The estimator works in totals (a byte heuristic over the whole prompt and response, and the
		// provider's reported totals above); the cache and reasoning parts leave them here. This is
		// the only subtraction in the ledger, and it applies to estimates only.
		p.input, p.output = max(r.InputTokens-p.cacheRead-p.cacheWrite, 0), max(r.OutputTokens-p.reasoning, 0)
		p.estimated = !p.waived
		p.aborted = x.Aborted && !x.Rejected && !x.Failed
		est := map[string]any{
			"method": r.Method, "input_reported": r.InputReported, "output_reported": r.OutputReported,
			"thinking_unmeasured": r.ThinkingUnmeasured, "max_output_fallback": r.MaxOutputFallback, "max_output_capped": r.MaxOutputCapped, "ratio_static": r.RatioStatic, "ratio_effective": r.RatioEffective,
			"learned_p90_tokens_per_byte": r.LearnedP90TokensPerByte, "overhead_tokens": r.OverheadTokens,
			"reserve_output_tokens": r.ReserveOutputTokens,
		}
		if r.OutputBasis != "" {
			// How the output of a response cut short was estimated (old issue 670).
			est["output_basis"] = r.OutputBasis
		}
		meta["estimate"] = est
		if p.aborted && r.ReserveOutputTokens > p.totalOut() {
			// The bound the gateway key holds back while the provider's figure is pending; never charged.
			bound, err := qt.Price(p.usage(max(r.ReserveOutputTokens-p.reasoning, 0)))
			if err != nil {
				return p, withRequest(e.RequestID, err)
			}
			p.reserve = bound.Cost // the charge is subtracted below, once it is known
		}
	}
	if e.Cut && e.InputKnown && e.OutputKnown {
		// A streamed Anthropic-format response cut short (its run's cancel, or a proxy error):
		// message_start logged usage that looks complete but is not (output 1). The input is charged
		// as logged and the output is the larger of the logged one and the output streamed, estimated
		// as for a cut stream without usage (ADR 0056, amendment 2026-10-31, point 2; old issue 670).
		// It is flagged usage_partial, and the max_tokens bound is held back from the key until the
		// provider's figure or the deadline.
		bound, fallback, capped := estimate.ReserveOutput(int64(res.Entry.MaxOutput), e.MaxTokens)
		params, err := prices.Calibrated(ctx, q, org, e.Model, res)
		if err != nil {
			return p, err
		}
		streamed, basis := estimate.CutOutput(params, max(e.ResponseBytes, 0), e.ResponseBytes >= 0, bound)
		// Only message_start's usage is a placeholder; any other logged usage is the provider's own
		// and is not raised.
		if streamed > p.totalOut() && e.Format == modelformat.Anthropic && e.Stream {
			p.output = max(streamed-p.reasoning, 0)
			p.estimated = true
		}
		p.aborted, p.flagged = true, true
		meta["usage_partial"] = true
		meta["estimate"] = map[string]any{
			"method": "partial", "reserve_output_tokens": bound, "max_output_fallback": fallback, "max_output_capped": capped,
			"output_basis": basis, "output_logged_tokens": e.OutputTokens + e.ReasoningTokens, "output_estimated": p.estimated,
		}
		if bound > p.totalOut() {
			b, err := qt.Price(p.usage(max(bound-p.reasoning, 0)))
			if err != nil {
				return p, withRequest(e.RequestID, err)
			}
			p.reserve = b.Cost
		}
	}
	u := p.usage(p.output)
	charged, err := qt.Price(u)
	if err != nil {
		return p, withRequest(e.RequestID, err)
	}
	cost := charged.Cost
	p.cost = cost
	p.reserve = p.reserve.SubFloor(cost)
	// Cost truth: our cost counts. A real (not estimated) request whose cost the gateway also
	// logged is compared with it.
	if !p.estimated && p.gatewayCost.Valid && differs(cost, gw) {
		p.flagged = true
		meta["gateway_diff"] = map[string]any{"cost_usd": cost.String(), "gateway_cost_usd": gw.String()}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return p, err
	}
	if p.estimated || p.flagged || res.Fallback || p.aborted || p.waived {
		p.estimate = raw
	}
	return p, nil
}

// withRequest names the request in a price error, so a flagged entry's price_error says which
// request it was. The error stays classifiable by [prices.Unpriceable].
func withRequest(requestID string, err error) error {
	return fmt.Errorf("price request %s: %w", requestID, err)
}

// differs reports |cost - gateway| > 1% of the gateway's cost (any difference when it is 0).
func differs(cost, gateway money.USD) bool {
	d := int64(cost - gateway)
	if d < 0 {
		d = -d
	}
	if gateway == 0 {
		return d != 0
	}
	diff := new(big.Rat).SetInt64(d)
	limit := new(big.Rat).Mul(FlagThreshold, new(big.Rat).SetInt64(int64(gateway)))
	return diff.Cmp(limit) > 0
}

// gatewayCost converts the gateway's decimal total (up to 18 fraction digits) to money, rounding
// half up at the twelfth digit.
func gatewayCost(s string) (money.USD, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || !decimalRE.MatchString(s) {
		return 0, fmt.Errorf("ledger: invalid gateway cost %q", s)
	}
	return money.FromRat(r)
}

// ValidGatewayCost reports whether s is a plain non-negative decimal a gateway may report as a cost
// (up to 10 integer and 18 fractional digits).
func ValidGatewayCost(s string) bool { return decimalRE.MatchString(s) }

var decimalRE = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,18})?$`)
