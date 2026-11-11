// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
)

// SettleWindow bounds how long a provisional entry waits for its provider's own figure. After it,
// the entry is final at its logged usage and its reserve is released. OpenRouter publishes a
// generation's stats within seconds to minutes of its end.
const SettleWindow = time.Hour

// settleBackoff is the wait before attempt n+1: 5 s, doubling, at most 5 minutes. About a dozen
// attempts fit in SettleWindow.
func settleBackoff(attempts int32) time.Duration {
	d := 5 * time.Second
	for range min(attempts, 6) {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// Settler makes provisional entries final at the provider's bill (ADR 0056, amendments 2026-10-28
// and 2026-11-06): every response of a provider that reports its own record (OpenRouter), complete
// or cut short. It asks the entry's provider for that record; the reported tokens replace the
// entry's and the billed cost (with the org's own upstream key, the aggregator's fee plus what the
// upstream billed that key) becomes the entry's cost. Koinon's price of the same tokens is kept
// beside it and the entry is flagged when the two differ by more than 1%, which shows where the
// price list is off. An entry whose provider has no figure by the deadline, or cannot report at
// all (a hold), becomes final at its logged usage, never max_tokens. Each change is recorded in spend_entry_revisions with the entry before and
// after. Several settlers may run: a row is claimed with SKIP LOCKED and leased. A pass asks at most
// MaxPerPass times, takes the orgs in turn from the longest waiting, and does not ask a provider
// that answered 429 before its Retry-After, across passes.
type Settler struct {
	DB *pgxpool.Pool
	// Keys opens the provider keys (the secret store's keyring).
	Keys catalog.Opener
	// Prices prices the provider's tokens for the comparison with its bill; required
	// (prices.NewResolver), the same list the Ingester prices with.
	Prices prices.Resolver
	// Sources looks responses up per provider type; nil is providerusage.Default.
	Sources providerusage.Registry
	Log     *slog.Logger
	// Notifier delivers budget alerts a settlement causes; nil stores in-app notifications.
	Notifier budget.Notifier
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Interval between passes; zero is 15 seconds.
	Interval time.Duration
	// MaxPerPass caps the provider lookups of one pass; zero is DefaultSettlePerPass. The rest wait
	// for the next pass.
	MaxPerPass int

	mu        sync.Mutex
	notBefore map[pgtype.UUID]time.Time // provider ID: do not ask before (a 429's Retry-After)
}

// DefaultSettlePerPass is the default cap on lookups per settler pass.
const DefaultSettlePerPass = 200

// settleLease is how long a claimed settlement is withheld from other settlers while its provider
// is asked (longer than the source's HTTP timeout).
const settleLease = 2 * time.Minute

// errHold is a settlement without a response ID: nothing to ask; it ends at its deadline.
var errHold = errors.New("the provider cannot report its usage; the reserve is held until the deadline")

// errNoSource means the entry can no longer be settled (its provider was deleted, changed type, or
// moved to a URL its source does not serve): it is final now.
var errNoSource = errors.New("no usage source serves the provider")

// errKeyLost means the provider's API key was lost with its key-encryption key (ADR 0085 decision
// 13): the provider is not asked without it. The attempt is deferred like a pending one, so a key
// entered again before the deadline still settles the entry; at the deadline the logged usage is
// final.
var errKeyLost = errors.New("the provider's API key was lost with a key-encryption key; enter it again to settle")

// Validate reports a wiring mistake: a Settler without prices could not compare what it settles.
func (s *Settler) Validate() error {
	if s.Prices.IsZero() {
		return errors.New("ledger: Settler.Prices is required (prices.NewResolver)")
	}
	return nil
}

func (s *Settler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Settler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Settler) sources() providerusage.Registry {
	if s.Sources != nil {
		return s.Sources
	}
	return providerusage.Default(nil)
}

// waitUntil is when the provider may be asked again; zero when it may now.
func (s *Settler) waitUntil(provider pgtype.UUID) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.notBefore[provider]; ok && s.now().Before(t) {
		return t
	}
	return time.Time{}
}

func (s *Settler) throttle(provider pgtype.UUID, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notBefore == nil {
		s.notBefore = map[pgtype.UUID]time.Time{}
	}
	if until.After(s.notBefore[provider]) {
		s.notBefore[provider] = until
	}
}

// Run settles due entries every Interval until ctx ends.
func (s *Settler) Run(ctx context.Context) {
	every := s.Interval
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.SettleDue(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("spend settler: pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SettleDue makes one pass over the entries due for an attempt and returns how many became final.
// It takes one entry per org in turn, the org with the longest-waiting entry first, so one busy org
// cannot use up a pass.
func (s *Settler) SettleDue(ctx context.Context) (int, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	orgs, err := store.New(s.DB).ListOrgsWithDueSettlements(ctx, pgtype.Timestamptz{Time: s.now(), Valid: true})
	if err != nil {
		return 0, fmt.Errorf("list orgs with due settlements: %w", err)
	}
	left := s.MaxPerPass
	if left <= 0 {
		left = DefaultSettlePerPass
	}
	done := 0
	var errs []error
	for len(orgs) > 0 && left > 0 {
		var next []pgtype.UUID
		for _, org := range orgs {
			if left <= 0 {
				break
			}
			claimed, settled, asked, err := s.settleNext(ctx, org)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if asked {
				left--
			}
			if settled {
				done++
			}
			if claimed {
				next = append(next, org)
			}
		}
		orgs = next
	}
	return done, errors.Join(errs...)
}

// settleNext claims and leases the org's next due row, asks its provider outside any transaction,
// then records the outcome. A settler that dies after the lease leaves the row due again when the
// lease ends. asked says whether a provider was contacted (what MaxPerPass counts).
func (s *Settler) settleNext(ctx context.Context, org pgtype.UUID) (claimed, settled, asked bool, err error) {
	var row store.ClaimDueSettlementsRow
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := store.New(tx)
		rows, err := q.ClaimDueSettlements(ctx, store.ClaimDueSettlementsParams{
			OrgID: org, Now: pgtype.Timestamptz{Time: s.now(), Valid: true}, MaxRows: 1,
		})
		if err != nil || len(rows) == 0 {
			return err
		}
		row, claimed = rows[0], true
		return q.LeaseSpendSettlement(ctx, store.LeaseSpendSettlementParams{
			OrgID: org, EntryID: row.EntryID, Until: pgtype.Timestamptz{Time: s.now().Add(settleLease), Valid: true},
		})
	})
	if err != nil || !claimed {
		return claimed, false, false, err
	}
	var u providerusage.Usage
	var lookupErr error
	switch {
	case !row.ResponseID.Valid:
		lookupErr = errHold
	default:
		if until := s.waitUntil(row.ProviderID); !until.IsZero() {
			lookupErr = &providerusage.RetryAfterError{After: until.Sub(s.now())}
			break
		}
		asked = true
		u, lookupErr = s.lookup(ctx, store.New(s.DB), org, row)
		var ra *providerusage.RetryAfterError
		if errors.As(lookupErr, &ra) {
			s.throttle(row.ProviderID, s.now().Add(ra.After))
		}
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		settled, err = s.settleOne(ctx, tx, org, row, u, lookupErr)
		return err
	})
	return true, settled, asked, err
}

// entrySnapshot is the part of an entry a settlement changes, for the revision.
type entrySnapshot struct {
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	ReasoningKnown   bool   `json:"reasoning_known"`
	CostUSD          string `json:"cost_usd"`
	ReserveUSD       string `json:"reserve_usd"`
	Provisional      bool   `json:"provisional"`
	// ProviderCostUSD is the provider's own cost figure, when it reported (with BYOK, the
	// aggregator's fee; CostUSD then adds the upstream's cost).
	ProviderCostUSD string `json:"provider_cost_usd,omitempty"`
	// KoinonCostUSD is Koinon's price of the reported tokens, kept for the comparison.
	KoinonCostUSD string `json:"koinon_cost_usd,omitempty"`
}

// settleOne records the outcome of asking the provider: its tokens and its bill replace the entry;
// or, for a hold, a provider no source serves any more, or past the deadline, the logged usage
// becomes final; or the attempt is deferred (no sooner than a provider's Retry-After).
func (s *Settler) settleOne(ctx context.Context, tx pgx.Tx, org pgtype.UUID, row store.ClaimDueSettlementsRow, u providerusage.Usage, lookupErr error) (bool, error) {
	q := store.New(tx)
	before := entrySnapshot{
		InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, CacheReadTokens: row.CacheReadTokens,
		CacheWriteTokens: row.CacheWriteTokens, ReasoningTokens: row.ReasoningTokens, ReasoningKnown: row.ReasoningKnown,
		CostUSD: row.CostUsd, ReserveUSD: row.ReserveUsd, Provisional: true,
	}
	meta := map[string]any{}
	if len(row.Estimate) > 0 {
		_ = json.Unmarshal(row.Estimate, &meta)
	}
	now := s.now()
	switch {
	case lookupErr == nil:
		// OpenRouter's generation reports no cache writes. The cache-write tokens the gateway
		// logged for the request (message_start reports them) are kept, within the provider's
		// prompt count; when neither source has them they are unknown, and the 1% flag against
		// the provider's bill marks the difference.
		if u.CacheWriteTokens == 0 && row.CacheWriteTokens > 0 {
			u.CacheWriteTokens = min(row.CacheWriteTokens, max(u.InputTokens-u.CacheReadTokens, 0))
		}
		// A generation without a reasoning figure keeps the split the gateway logged, within the
		// provider's output count; when neither reported one it stays unknown (#78).
		if !u.ReasoningKnown && row.ReasoningKnown {
			u.ReasoningTokens, u.ReasoningKnown = min(row.ReasoningTokens, u.OutputTokens), true
		}
		cost, koinon, ref, flagged, priceNote, err := s.charge(ctx, q, org, row, u)
		if err != nil {
			return false, err
		}
		after := entrySnapshot{
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens, ReasoningKnown: u.ReasoningKnown,
			CostUSD: cost.Numeric(), ReserveUSD: money.USD(0).Numeric(), ProviderCostUSD: u.Cost.Numeric(),
		}
		settled := map[string]any{
			"source": row.ProviderType, "response_id": row.ResponseID.String, "provisional_cost_usd": row.CostUsd,
			"provider_cost_usd": u.Cost.String(), "cost_usd": cost.String(), "charged": "provider", "cancelled": u.Cancelled,
		}
		if koinon != nil {
			after.KoinonCostUSD = koinon.Numeric()
			settled["koinon_cost_usd"] = koinon.String()
		}
		if u.BYOK {
			settled["byok"], settled["upstream_cost_usd"] = true, u.UpstreamCost.String()
		}
		for k, v := range priceNote {
			settled[k] = v
		}
		meta["settled"] = settled
		raw, err := json.Marshal(meta)
		if err != nil {
			return false, err
		}
		updated, err := s.finish(ctx, q, org, row, "provider", row.ProviderType, before, after, false, flagged, ref, raw)
		if err != nil {
			return false, err
		}
		// A settlement that raised the entry's cost can reach a limit: the agent's, the org's and the
		// platform's budgets and then the run's own, in the order and with the naming of an
		// ingested entry (ADR 0081, decision 5). Tokens the provider reports beyond the logged usage
		// are added to the run's reported total, once: only the settlement that changed the entry
		// adds them.
		if updated {
			if err := s.enforceSettled(ctx, tx, org, row, cost, max((u.InputTokens+u.OutputTokens)-(row.InputTokens+row.OutputTokens), 0)); err != nil {
				return false, err
			}
		}
		return updated, nil
	case errors.Is(lookupErr, errHold), errors.Is(lookupErr, errNoSource), !now.Before(row.Deadline.Time):
		// A hold at its deadline, an entry nothing can settle any more, or the window is over: the
		// logged usage is final; the reserve is released.
		after := before
		after.ReserveUSD, after.Provisional = money.USD(0).Numeric(), false
		meta["settled"] = map[string]any{"source": "gateway", "response_id": row.ResponseID.String, "expired": true, "reason": lookupErr.Error()}
		raw, err := json.Marshal(meta)
		if err != nil {
			return false, err
		}
		updated, err := s.finish(ctx, q, org, row, "expired", "gateway", before, after, true, false, pgtype.Text{}, raw)
		if err != nil {
			return false, err
		}
		if !errors.Is(lookupErr, errHold) {
			s.log().Info("spend settler: no provider figure; the logged usage is final",
				"org", org.String(), "entry", row.EntryID.String(), "provider_type", row.ProviderType, "err", lookupErr)
		}
		return updated, nil
	default:
		wait := settleBackoff(row.Attempts + 1)
		var ra *providerusage.RetryAfterError
		if errors.As(lookupErr, &ra) {
			wait = max(wait, ra.After)
		}
		return false, q.DeferSpendSettlement(ctx, store.DeferSpendSettlementParams{
			OrgID: org, EntryID: row.EntryID,
			NextAttemptAt: pgtype.Timestamptz{Time: now.Add(wait), Valid: true},
			LastError:     pgtype.Text{String: lookupErr.Error(), Valid: true},
		})
	}
}

// enforceSettled runs the checks of a recorded entry for one whose settlement changed it: the run's
// reported tokens grow by tokens (what the provider reports beyond the logged usage), then [enforce].
func (s *Settler) enforceSettled(ctx context.Context, tx pgx.Tx, org pgtype.UUID, row store.ClaimDueSettlementsRow, cost money.USD, tokens int64) error {
	q := store.New(tx)
	x := enforcement{org: org, agent: row.AgentID, run: row.RunID, at: row.OccurredAt.Time}
	if before, err := money.ParseNumeric(row.CostUsd); err != nil {
		return err
	} else {
		x.delta = cost.SubFloor(before)
	}
	if !x.agent.Valid {
		// An entry of a system key (no agent, no run): the org's limits and the key's own budget.
		return enforceSystem(ctx, tx, s.Notifier, s.now(), org, row.OccurredAt.Time, x.delta)
	}
	if row.RunID.Valid {
		run, err := q.GetRun(ctx, store.GetRunParams{OrgID: org, ID: row.RunID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the entry's run: %w", err)
		}
		x.version = run.AgentVersionID
		if _, err := q.AddRunTokens(ctx, store.AddRunTokensParams{OrgID: org, ID: row.RunID, Tokens: tokens}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("record run tokens: %w", err)
		}
	}
	return enforce(ctx, tx, s.Notifier, s.now(), x)
}

// charge is what a settled entry costs: the provider's bill, [providerusage.Usage.Billed] (ADR 0056,
// amendment 2026-11-06: billed cost counts, an org price override included). Koinon's price of the
// reported tokens, valid at the request's time, is computed beside it (koinon; nil without a usable
// price) and the entry is flagged when the two differ by more than 1%. The price is resolved again
// for the request's time, which gives the entry's price_ref unless the org's prices changed since
// (the note says so); the ref names the price the bill was compared with.
func (s *Settler) charge(ctx context.Context, q *store.Queries, org pgtype.UUID, row store.ClaimDueSettlementsRow,
	u providerusage.Usage,
) (cost money.USD, koinon *money.USD, ref pgtype.Text, flagged bool, note map[string]any, err error) {
	note = map[string]any{}
	if cost, err = u.Billed(); err != nil {
		return 0, nil, ref, false, nil, fmt.Errorf("the provider's bill: %w", err)
	}
	qt, err := s.Prices.Find(ctx, q, org, row.Model, row.OccurredAt.Time)
	var listed prices.Priced
	if err == nil {
		listed, err = qt.Price(u.Buckets())
	}
	if err != nil {
		reason, ok := prices.Unpriceable(err)
		switch {
		case !ok:
			return 0, nil, ref, false, nil, err
		case reason == prices.ReasonNoPrice:
			// No Koinon price: the bill needs none; nothing to compare.
			return cost, nil, ref, false, note, nil
		default:
			// A stored price that cannot be used is flagged, as at ingest; the bill still counts.
			note["price_error"] = err.Error()
			return cost, nil, ref, true, note, nil
		}
	}
	ref = pgtype.Text{String: qt.Resolution.Ref, Valid: true}
	if qt.Resolution.Ref != row.PriceRef && row.PriceRef != "" {
		note["price_ref_was"] = row.PriceRef
	}
	if differs(listed.Cost, cost) {
		flagged = true
		note["provider_diff"] = map[string]any{"koinon_cost_usd": listed.Cost.String(), "provider_cost_usd": cost.String()}
	}
	return cost, &listed.Cost, ref, flagged, note, nil
}

// lookup asks the entry's provider for its record of the response.
func (s *Settler) lookup(ctx context.Context, q *store.Queries, org pgtype.UUID, row store.ClaimDueSettlementsRow) (providerusage.Usage, error) {
	prov, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: row.ProviderID})
	if errors.Is(err, pgx.ErrNoRows) {
		return providerusage.Usage{}, errNoSource
	} else if err != nil {
		return providerusage.Usage{}, fmt.Errorf("read provider: %w", err)
	}
	if prov.AdminDisabledAt.Valid {
		// An admin switched the provider off (ADR 0110 decision 5): its usage API is not called. Like any
		// failed lookup the entry is asked again until its deadline, then the logged usage is final.
		return providerusage.Usage{}, outward.ErrDisabled
	}
	src, ok := s.sources().For(prov.Type, prov.Url, row.ResponseID.String)
	if !ok {
		return providerusage.Usage{}, errNoSource
	}
	if prov.CredentialLostAt.Valid {
		return providerusage.Usage{}, errKeyLost
	}
	key, err := catalog.OpenKey(s.Keys, org, prov.ID, prov.ApiKey)
	if err != nil {
		return providerusage.Usage{}, fmt.Errorf("open provider key: %w", err)
	}
	return src.Usage(ctx, providerusage.Provider{Type: prov.Type, URL: prov.Url, APIKey: key}, row.ResponseID.String)
}

// finish writes the entry's final figures, the revision and the end of the settlement in one
// transaction, and wakes the reconciler, which recomputes the key's pushed limit (the reserve is
// gone, the charge may differ) and stops the runs first when real charges spend a limit. updated
// is false when the entry was already final, so the caller adds nothing to the run's tokens.
func (s *Settler) finish(ctx context.Context, q *store.Queries, org pgtype.UUID, row store.ClaimDueSettlementsRow,
	reason, source string, before, after entrySnapshot, estimated, flagged bool, ref pgtype.Text, meta []byte,
) (updated bool, err error) {
	if _, err := q.SettleSpendEntry(ctx, store.SettleSpendEntryParams{
		OrgID: org, ID: row.EntryID,
		InputTokens: after.InputTokens, OutputTokens: after.OutputTokens,
		CacheReadTokens: after.CacheReadTokens, CacheWriteTokens: after.CacheWriteTokens, ReasoningTokens: after.ReasoningTokens,
		ReasoningKnown: after.ReasoningKnown,
		CostUsd:        after.CostUSD, PriceRef: ref, Estimated: estimated, Estimate: meta, CostFlagged: flagged,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("settle spend entry: %w", err)
	} else if err == nil {
		updated = true
		b, err := json.Marshal(before)
		if err != nil {
			return false, err
		}
		a, err := json.Marshal(after)
		if err != nil {
			return false, err
		}
		if err := q.InsertSpendEntryRevision(ctx, store.InsertSpendEntryRevisionParams{
			OrgID: org, EntryID: row.EntryID, Reason: reason, Source: source, Before: b, After: a,
		}); err != nil {
			return false, fmt.Errorf("record revision: %w", err)
		}
	}
	// No row: the entry was already final (another settler, or a replay); its settlement just ends.
	if err := q.DeleteSpendSettlement(ctx, store.DeleteSpendSettlementParams{OrgID: org, EntryID: row.EntryID}); err != nil {
		return false, fmt.Errorf("delete settlement: %w", err)
	}
	return updated, q.NotifyReconciler(ctx, org)
}
