// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/pricing"
)

// Triggers of a fetch, as price_source_fetches.trigger records them.
const (
	TriggerSchedule = "schedule"
	TriggerCLI      = "cli"
	TriggerImport   = "import"
)

// Origins of a proposal.
const (
	OriginFetch  = "fetch"
	OriginImport = "import"
)

// Proposal states.
const (
	StateOpen       = "open"
	StateAccepted   = "accepted"
	StateDismissed  = "dismissed"
	StateSuperseded = "superseded"
)

// Proposal is a price proposal as callers see it (ADR 0098 decision 2.6).
type Proposal struct {
	ID           string              `json:"id"`
	Source       string              `json:"source"`
	Origin       string              `json:"origin"`
	ProviderType string              `json:"providerType"`
	Model        string              `json:"model"`
	CurrentRef   string              `json:"currentRef,omitempty"`
	Current      *pricing.Entry      `json:"current,omitempty"`
	Proposed     *pricing.Entry      `json:"proposed,omitempty"`
	Changes      []pricefetch.Change `json:"changes"`
	Provenance   Provenance          `json:"provenance"`
	Agreeing     []string            `json:"agreeingSources"`
	Class        pricefetch.Class    `json:"class"`
	FailedRules  string              `json:"failedRules,omitempty"`
	State        string              `json:"state"`
	CreatedAt    time.Time           `json:"createdAt"`
	DecidedAt    *time.Time          `json:"decidedAt,omitempty"`
	DecidedBy    string              `json:"decidedBy,omitempty"`
	Applied      int64               `json:"appliedVersion,omitempty"`
}

func toProposal(r store.PriceProposal) (Proposal, error) {
	p := Proposal{
		ID: r.ID.String(), Source: r.Source, Origin: r.Origin, ProviderType: r.ProviderType, Model: r.Model, CurrentRef: r.CurrentRef,
		Agreeing: r.AgreeingSources, Class: pricefetch.Class(r.Class), FailedRules: r.FailedRules, State: r.State, CreatedAt: r.CreatedAt.Time,
		DecidedBy: r.DecidedBy, Applied: r.AppliedVersion.Int64,
	}
	if r.DecidedAt.Valid {
		t := r.DecidedAt.Time
		p.DecidedAt = &t
	}
	if len(r.CurrentEntry) > 0 {
		p.Current = new(pricing.Entry)
		if err := json.Unmarshal(r.CurrentEntry, p.Current); err != nil {
			return Proposal{}, err
		}
	}
	if len(r.ProposedEntry) > 0 {
		p.Proposed = new(pricing.Entry)
		if err := json.Unmarshal(r.ProposedEntry, p.Proposed); err != nil {
			return Proposal{}, err
		}
	}
	if err := json.Unmarshal(r.Changes, &p.Changes); err != nil {
		return Proposal{}, err
	}
	return p, json.Unmarshal(r.Provenance, &p.Provenance)
}

// RefreshConfig configures a Refresher.
type RefreshConfig struct {
	// Adapters are the sources it fetches. OpenRouter only, so far.
	Adapters []pricefetch.Adapter
	// Bounds tighten the auto-apply bounds; the zero value means the defaults.
	Bounds  pricefetch.Bounds
	Metrics *Metrics
	Log     *slog.Logger
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

// Refresher fetches the public sources, makes proposals from them and applies the ones inside the
// auto-apply bounds. Fetching happens only when a caller asks (the CLI) or a tick of Loop finds a
// source its RefreshMode allows (ADR 0098 decision 2.2 and its 2026-11-07 amendment).
type Refresher struct {
	svc      *Service
	adapters map[string]pricefetch.Adapter
	bounds   pricefetch.Bounds
	metrics  *Metrics
	log      *slog.Logger
	now      func() time.Time
}

// NewRefresher returns a Refresher over svc. A bound looser than the default is an error.
func NewRefresher(svc *Service, cfg RefreshConfig) (*Refresher, error) {
	b := cfg.Bounds
	if b == (pricefetch.Bounds{}) {
		b = pricefetch.DefaultBounds
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	r := &Refresher{svc: svc, adapters: map[string]pricefetch.Adapter{}, bounds: b, metrics: cfg.Metrics, log: cfg.Log, now: cfg.Now}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	for _, a := range cfg.Adapters {
		r.adapters[a.Name()] = a
	}
	return r, nil
}

// Service returns the Service the Refresher reads and writes through.
func (r *Refresher) Service() *Service { return r.svc }

// effective is the entry that prices a model with no org entry: the platform entry, else the
// default one.
type effective struct {
	Entry    pricing.Entry
	Ref      string
	Reviewed pricing.Spec
}

type priceView struct {
	version     int64
	plat        []pricing.Resolved
	platByID    map[string]PlatformEntry
	listVersion string
	defs        []pricing.Resolved
}

func (s *Service) view(ctx context.Context, q *store.Queries, now time.Time) (priceView, error) {
	pv, _, err := platformVersionAt(ctx, q, now)
	if err != nil {
		return priceView{}, err
	}
	v := priceView{version: pv.Version, platByID: map[string]PlatformEntry{}}
	if pv.Version > 0 {
		_, v.plat, err = platformEntries(ctx, q, now)
		if err != nil {
			return priceView{}, err
		}
		for _, e := range pv.Entries {
			v.platByID[e.Entry.ID] = e
		}
	}
	list, defs := s.src.Current()
	v.listVersion, v.defs = list.Version, defs
	return v, nil
}

func (v priceView) effective(providerType, model string, at time.Time) *effective {
	if r, ok := pricing.Select(v.plat, providerType, model, at); ok {
		return &effective{Entry: r.Entry, Ref: PlatformRef(v.version, r.Entry.ID), Reviewed: v.platByID[r.Entry.ID].Reviewed}
	}
	if r, ok := pricing.Select(v.defs, providerType, model, at); ok {
		return &effective{Entry: r.Entry, Ref: pricing.Ref(v.listVersion, r.Entry.ID), Reviewed: r.Entry.Spec}
	}
	return nil
}

// literalModels are the model names that entries of a provider type price one by one.
func (v priceView) literalModels(providerType string) []string {
	var out []string
	for _, rs := range [][]pricing.Resolved{v.plat, v.defs} {
		for _, r := range rs {
			g := r.Entry.Match.Glob
			if r.Entry.Provider == providerType && g != "" && !strings.ContainsAny(g, "*?") {
				out = append(out, g)
			}
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// RunResult says what one run did.
type RunResult struct {
	Fetched   []string
	Failed    map[string]string
	Proposals int
	Applied   int
}

// Run fetches every adapter, records the fetch, makes proposals and, when auto is true, applies
// the proposals inside the held bounds (heldBounds: never looser than the recorded ones). A failing
// source does not stop the others. The proposals are classified under the held bounds, auto or not.
func (r *Refresher) Run(ctx context.Context, trigger string, auto bool) (RunResult, error) {
	b, err := r.heldBounds(ctx)
	if err != nil {
		return RunResult{Failed: map[string]string{}}, err
	}
	return r.run(ctx, trigger, r.adapterNames(), auto, b)
}

func (r *Refresher) adapterNames() []string {
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// run fetches the named adapters. Auto-apply then considers only the proposals of the sources that
// were fetched successfully in this run: a source that failed proposes nothing new, and its older
// proposals wait for the next good fetch. Auto-apply holds the proposals to bounds b, which every
// caller takes from the recorded setting (heldBounds, or Effective for a scheduled tick).
func (r *Refresher) run(ctx context.Context, trigger string, names []string, auto bool, b pricefetch.Bounds) (RunResult, error) {
	res := RunResult{Failed: map[string]string{}}
	for _, name := range names {
		n, err := r.fetchOne(ctx, r.adapters[name], trigger, b)
		if err != nil {
			res.Failed[name] = err.Error()
			r.log.Warn("price source fetch failed", "source", name, "err", err)
			continue
		}
		res.Fetched = append(res.Fetched, name)
		res.Proposals += n
	}
	if auto && len(res.Fetched) > 0 {
		n, err := r.autoApply(ctx, b, res.Fetched...)
		res.Applied = n
		if err != nil {
			return res, err
		}
	}
	if err := r.metrics.refreshProposals(ctx, store.New(r.svc.pool)); err != nil {
		r.log.Warn("price proposal metrics", "err", err)
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("prices: %d source(s) failed: %v", len(res.Failed), res.Failed)
	}
	return res, nil
}

// minModelsOf is the model-count floor of an adapter.
func minModelsOf(a pricefetch.Adapter) int {
	if f, ok := a.(interface{ MinModels() int }); ok {
		return f.MinModels()
	}
	return 1
}

func (r *Refresher) fetchOne(ctx context.Context, a pricefetch.Adapter, trigger string, b pricefetch.Bounds) (int, error) {
	now := r.now()
	q := store.New(r.svc.pool)
	lastGood := 0
	if f, err := q.GetLastGoodPriceSourceFetch(ctx, a.Name()); err == nil {
		lastGood = int(f.ModelCount)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	record := func(result, errText string, snap pricefetch.Snapshot) error {
		_, err := q.InsertPriceSourceFetch(ctx, store.InsertPriceSourceFetchParams{
			Source: a.Name(), Url: snap.URL, FetchedAt: pgtype.Timestamptz{Time: now, Valid: true}, Result: result, Error: truncate(errText, 1900),
			Sha256: snap.SHA256, Etag: truncate(snap.ETag, 290), CommitSha: truncate(snap.CommitSHA, 60), ModelCount: int32(snap.Count), Trigger: trigger, // #nosec G115 -- bounded by the body cap
		})
		r.metrics.fetched(a.Name(), result)
		return err
	}
	snap, err := a.Fetch(ctx, now)
	if err != nil {
		if rerr := record(FetchFailed, err.Error(), pricefetch.Snapshot{}); rerr != nil {
			return 0, errors.Join(err, rerr)
		}
		return 0, err
	}
	if err := pricefetch.CheckFloor(snap, minModelsOf(a), lastGood); err != nil {
		if rerr := record(FetchRefused, err.Error(), snap); rerr != nil {
			return 0, errors.Join(err, rerr)
		}
		return 0, err
	}
	n := 0
	err = pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if _, err := tq.InsertPriceSourceFetch(ctx, store.InsertPriceSourceFetchParams{
			Source: a.Name(), Url: snap.URL, FetchedAt: pgtype.Timestamptz{Time: now, Valid: true}, Result: FetchOK, Sha256: snap.SHA256,
			Etag: truncate(snap.ETag, 290), CommitSha: truncate(snap.CommitSHA, 60), ModelCount: int32(snap.Count), Trigger: trigger, // #nosec G115 -- bounded by the body cap
		}); err != nil {
			return err
		}
		var err error
		n, err = r.propose(ctx, tq, snap, OriginFetch, "", now, b)
		return err
	})
	if err != nil {
		return 0, err
	}
	r.metrics.fetched(a.Name(), FetchOK)
	return n, nil
}

// propose diffs a snapshot against the effective prices and stores the proposals, classified under
// bounds (the held ones, so a stored class says what the auto-apply will find). It returns how
// many proposals it created or refreshed. For a fetch it first records the observations that the
// stability rule reads.
func (r *Refresher) propose(ctx context.Context, q *store.Queries, snap pricefetch.Snapshot, origin, vouchedBy string, now time.Time, bounds pricefetch.Bounds) (int, error) {
	v, err := r.svc.view(ctx, q, now)
	if err != nil {
		return 0, err
	}
	offers := map[string]pricefetch.Offer{}
	providerTypes := map[string]bool{}
	for _, o := range snap.Offers {
		offers[o.ProviderType+"\x00"+o.Model] = o
		providerTypes[o.ProviderType] = true
	}
	n := 0
	for pt := range providerTypes {
		catalog := map[string]bool{}
		names, err := q.ListPriceModelNames(ctx, pt)
		if err != nil {
			return 0, err
		}
		cands := map[string]bool{}
		for _, m := range names {
			catalog[m], cands[m] = true, true
		}
		for _, m := range v.literalModels(pt) {
			cands[m] = true
		}
		models := make([]string, 0, len(cands))
		for m := range cands {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			o, ok := offers[pt+"\x00"+m]
			if !ok {
				continue // a model missing from the source never changes a price
			}
			made, err := r.proposeModel(ctx, q, v, snap, o, origin, vouchedBy, now, bounds)
			if err != nil {
				return 0, err
			}
			if made {
				n++
			}
		}
	}
	return n, nil
}

func (r *Refresher) proposeModel(ctx context.Context, q *store.Queries, v priceView, snap pricefetch.Snapshot, o pricefetch.Offer, origin, vouchedBy string, now time.Time, bounds pricefetch.Bounds) (bool, error) {
	cur := v.effective(o.ProviderType, o.Model, now)
	var curEntry *pricing.Entry
	if cur != nil {
		curEntry = &cur.Entry
	}
	hash := pricefetch.SpecHash(o.Spec)
	if len(o.Problems) > 0 {
		hash = pricefetch.DigestBody([]byte(strings.Join(o.Problems, "\n")))
	}
	stable := false
	if origin == OriginFetch {
		obs, err := q.ObservePriceValue(ctx, store.ObservePriceValueParams{Source: snap.Source, Model: o.Model, ValuesHash: hash, At: pgtype.Timestamptz{Time: now, Valid: true}})
		if err != nil {
			return false, err
		}
		stable = obs.Fetches >= 2 && obs.LastSeen.Time.Sub(obs.FirstSeen.Time) >= pricefetch.StabilityWindow
	}
	prov := Provenance{
		Source: snap.Source, URL: snap.URL, FetchedAt: snap.FetchedAt, SHA256: snap.SHA256, CommitSHA: snap.CommitSHA, ETag: snap.ETag,
		Licence: snap.Licence, AgreeingSources: []string{snap.Source}, VouchedBy: vouchedBy, Ignored: o.Ignored,
	}
	provJSON, err := json.Marshal(prov)
	if err != nil {
		return false, err
	}
	var curJSON []byte
	if curEntry != nil {
		if curJSON, err = json.Marshal(curEntry); err != nil {
			return false, err
		}
	}
	params := store.UpsertPriceProposalParams{
		Source: snap.Source, Origin: origin, ProviderType: o.ProviderType, Model: o.Model, CurrentEntry: curJSON, Provenance: provJSON,
		AgreeingSources: []string{snap.Source}, ValuesHash: hash, Changes: []byte("[]"),
	}
	if cur != nil {
		params.CurrentRef = cur.Ref
	}
	entry, err := pricefetch.BuildEntry(o, curEntry, snap.Source, now)
	noMaxOutput := errors.Is(err, pricefetch.ErrNoMaxOutput)
	if err != nil && !noMaxOutput {
		params.Class, params.FailedRules = string(pricefetch.ClassRefused), truncate("refused: "+err.Error(), 3900)
	} else {
		if curEntry != nil && !pricefetch.Differs(curEntry.Spec, entry.Spec) {
			// The source agrees with the price in use: nothing to propose, and an older proposal for
			// other values is stale.
			_, err := q.SupersedeOpenPriceProposals(ctx, store.SupersedeOpenPriceProposalsParams{Source: snap.Source, Model: o.Model, Origin: origin, KeepHash: ""})
			return false, err
		}
		verdict := pricefetch.Classify(pricefetch.Facts{
			Origin: origin, Source: snap.Source, Current: curEntry, Baseline: baselineOf(cur), Proposed: entry,
			Agreeing: []string{snap.Source}, Stable: stable,
		}, r.bounds)
		if noMaxOutput {
			// Never auto: a person supplies the output limit when accepting.
			verdict.Failed = append([]string{pricefetch.ErrNoMaxOutput.Error()}, verdict.Failed...)
			if verdict.Class == pricefetch.ClassAuto {
				verdict.Class = pricefetch.ClassNeedsReview
			}
		}
		params.Class, params.FailedRules = string(verdict.Class), truncate(strings.Join(verdict.Failed, "; "), 3900)
		if params.ProposedEntry, err = json.Marshal(entry); err != nil {
			return false, err
		}
		if params.Changes, err = json.Marshal(append([]pricefetch.Change{}, verdict.Changes...)); err != nil {
			return false, err
		}
	}
	if _, err := q.SupersedeOpenPriceProposals(ctx, store.SupersedeOpenPriceProposalsParams{Source: snap.Source, Model: o.Model, Origin: origin, KeepHash: hash}); err != nil {
		return false, err
	}
	if _, err := q.UpsertPriceProposal(ctx, params); errors.Is(err, pgx.ErrNoRows) {
		return false, nil // dismissed before, for these values: remembered
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func baselineOf(e *effective) *pricing.Spec {
	if e == nil {
		return nil
	}
	return &e.Reviewed
}

// AutoApply applies the open proposals of class auto in one platform version (actor system koinon-api:price-refresh), unless
// the day's cap would be exceeded, in which case none applies and they wait for a person. With
// sources, only those sources' proposals are considered. Each candidate is classified again here,
// under the held bounds (the tighter of this Refresher's and the recorded ones, heldBounds), against
// the price in use and the stored stability of its values: a proposal classified earlier under
// looser bounds (a CLI fetch, another replica, an older setting) applies only if it still is auto,
// and otherwise moves to the class it now has. No caller can apply under bounds looser than the
// recorded setting says (#328 review). It returns how many it applied.
func (r *Refresher) AutoApply(ctx context.Context, sources ...string) (int, error) {
	b, err := r.heldBounds(ctx)
	if err != nil {
		return 0, err
	}
	return r.autoApply(ctx, b, sources...)
}

// autoApply is AutoApply under bounds b, which the caller read from the recorded setting.
func (r *Refresher) autoApply(ctx context.Context, b pricefetch.Bounds, sources ...string) (int, error) {
	applied := 0
	_, err := audit.InTx(ctx, r.svc.pool, func(q *store.Queries, ab audit.Batch) error {
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		rows, err := q.ListOpenAutoPriceProposals(ctx)
		if err != nil {
			return err
		}
		now := r.now()
		v, err := r.svc.view(ctx, q, now)
		if err != nil {
			return err
		}
		var cands []Proposal
		for _, row := range rows {
			if len(sources) > 0 && !slices.Contains(sources, row.Source) {
				continue
			}
			p, err := toProposal(row)
			if err != nil {
				return err
			}
			// Only a proposal still diffed against the price in use applies; a new model's proposal
			// applies only while the model still has no price.
			cur := v.effective(p.ProviderType, p.Model, now)
			if (cur == nil) != (p.Current == nil) || (cur != nil && !sameSpec(cur.Entry.Spec, p.Current.Spec)) || p.Proposed == nil {
				continue
			}
			var curEntry *pricing.Entry
			if cur != nil {
				curEntry = &cur.Entry
			}
			stable, err := storedStable(ctx, q, row.Source, row.Model, row.ValuesHash)
			if err != nil {
				return err
			}
			verdict := pricefetch.Classify(pricefetch.Facts{
				Origin: p.Origin, Source: p.Source, Current: curEntry, Baseline: baselineOf(cur), Proposed: *p.Proposed,
				Agreeing: p.Agreeing, Stable: stable,
			}, b)
			if verdict.Class != pricefetch.ClassAuto {
				if err := q.SetPriceProposalClass(ctx, store.SetPriceProposalClassParams{
					ID: row.ID, Class: string(verdict.Class), FailedRules: truncate(strings.Join(verdict.Failed, "; "), 3900),
				}); err != nil {
					return err
				}
				continue
			}
			cands = append(cands, p)
		}
		if len(cands) == 0 {
			return nil
		}
		since := pgtype.Timestamptz{Time: now.Add(-24 * time.Hour), Valid: true}
		today, err := q.CountAutoAppliedPriceProposalsSince(ctx, store.CountAutoAppliedPriceProposalsSinceParams{DecidedBy: AutoActor.String(), Since: since})
		if err != nil {
			return err
		}
		if !pricefetch.CapAllows(int(today), len(cands), b.MaxAutoPerDay) {
			why := fmt.Sprintf("daily cap: %d automatic change(s) in the last 24 hours and %d more waiting exceed the cap of %d; a large batch looks like a broken source, so all of them wait for a person", today, len(cands), b.MaxAutoPerDay)
			for _, p := range cands {
				id, _ := parseUUID(p.ID)
				if err := q.SetPriceProposalClass(ctx, store.SetPriceProposalClassParams{ID: id, Class: string(pricefetch.ClassNeedsReview), FailedRules: why}); err != nil {
					return err
				}
			}
			return nil
		}
		a, err := r.applyEntries(ctx, q, v, cands, AutoActor, now)
		if err != nil {
			return err
		}
		if a.Dropped > 0 {
			// The new version would leave out the bad entries, a change no rule allows the auto-apply
			// to make. The proposals stay open until a person rolls back or replaces the version.
			r.log.Warn("automatic price updates wait: the current platform version has entries that no longer decode or validate",
				"proposals", len(cands), "entries", a.Dropped)
			return nil
		}
		pv, err := writeVersion(ctx, q, ab, versionWrite{
			Actor: AutoActor, Action: "platform_price.set", Reason: fmt.Sprintf("%d automatic price update(s) within bounds", len(cands)),
			RuleSet: pricefetch.RuleSetVersion, ProposalIDs: a.IDs, Entries: a.Entries, Changed: a.Changed,
		})
		if err != nil {
			return err
		}
		for _, p := range cands {
			if err := r.decide(ctx, q, ab, p, StateAccepted, AutoActor, pv.Version); err != nil {
				return err
			}
			r.metrics.autoApplied(p.Source)
		}
		applied = len(cands)
		return nil
	})
	if applied > 0 {
		r.log.Info("platform prices updated automatically", "entries", applied)
	}
	return applied, err
}

// storedStable applies the stability rule to the values a proposal carries: the source's latest run
// of unchanged fetches for the model has these values and spans two fetches StabilityWindow apart.
func storedStable(ctx context.Context, q *store.Queries, source, model, valuesHash string) (bool, error) {
	obs, err := q.GetPriceSourceObservation(ctx, store.GetPriceSourceObservationParams{Source: source, Model: model})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return obs.ValuesHash == valuesHash && obs.Fetches >= 2 && obs.LastSeen.Time.Sub(obs.FirstSeen.Time) >= pricefetch.StabilityWindow, nil
}

func (m *Metrics) autoApplied(source string) {
	if m != nil {
		m.AutoApplied.WithLabelValues(source).Inc()
	}
}

func sameSpec(a, b pricing.Spec) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	err := u.Scan(s)
	return u, err
}

// applyEntries returns the platform entries after applying the proposals to the current version:
// each proposal's entry replaces the platform entry of its id or is added. A person's acceptance
// sets the reviewed price to the proposed one; an automatic one keeps the last reviewed price. A
// new model applied automatically has no reviewed price yet, so its first applied price becomes the
// baseline later automatic changes are bounded against, until a person reviews one.
func (r *Refresher) applyEntries(ctx context.Context, q *store.Queries, v priceView, ps []Proposal, actor audit.Actor, now time.Time) (applied, error) {
	cur, err := currentPlatform(ctx, q)
	if err != nil {
		return applied{}, err
	}
	byID := map[string]PlatformEntry{}
	for _, e := range cur.Entries {
		byID[e.Entry.ID] = e
	}
	var ids []pgtype.UUID
	var changed []string
	for _, p := range ps {
		if p.Proposed == nil {
			return applied{}, &InvalidError{Msg: "proposal " + p.ID + " has no entry"}
		}
		e := *p.Proposed
		reviewed := e.Spec
		if actor == AutoActor {
			if old, ok := byID[e.ID]; ok {
				reviewed = old.Reviewed
			} else if cur := v.effective(p.ProviderType, p.Model, now); cur != nil {
				reviewed = cur.Reviewed
			}
		}
		prov := p.Provenance
		prov.ProposalID, prov.Approver, prov.ApprovedAt = p.ID, actor.String(), now.UTC()
		if actor == AutoActor {
			prov.RuleSetVersion = pricefetch.RuleSetVersion
		}
		byID[e.ID] = PlatformEntry{Entry: e, Reviewed: reviewed, Provenance: prov}
		changed = append(changed, e.ID)
		id, err := parseUUID(p.ID)
		if err != nil {
			return applied{}, err
		}
		ids = append(ids, id)
	}
	out := make([]PlatformEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	return applied{Entries: out, IDs: ids, Changed: changed, Dropped: cur.BadEntries}, nil
}

// applied is a new platform version's content made by applyEntries.
type applied struct {
	Entries []PlatformEntry
	IDs     []pgtype.UUID
	Changed []string
	// Dropped counts the current version's entries that no longer decode or validate and so are not
	// carried into the new version.
	Dropped int
}

// decide marks a proposal accepted or dismissed and audits it. A proposal already decided is an
// InvalidError.
func (r *Refresher) decide(ctx context.Context, q *store.Queries, b audit.Batch, p Proposal, state string, actor audit.Actor, version int64) error {
	id, err := parseUUID(p.ID)
	if err != nil {
		return err
	}
	params := store.DecidePriceProposalParams{ID: id, State: state, DecidedBy: actor.String()}
	if version > 0 {
		params.AppliedVersion = pgtype.Int8{Int64: version, Valid: true}
	}
	if _, err := q.DecidePriceProposal(ctx, params); errors.Is(err, pgx.ErrNoRows) {
		return &InvalidError{Msg: "proposal " + p.ID + " was already decided"}
	} else if err != nil {
		return err
	}
	action := "price.proposal.accepted"
	if state == StateDismissed {
		action = "price.proposal.dismissed"
	}
	after := map[string]any{"model": p.Model, "source": p.Source, "class": p.Class, "changes": p.Changes}
	if version > 0 {
		after["platformVersion"] = version
	}
	auditPlatform(b, actor, action, "price_proposal", p.ID, nil, after)
	return nil
}

// ListProposals lists proposals, newest first; empty state or class mean all.
func (s *Service) ListProposals(ctx context.Context, state, class string, limit int) ([]Proposal, error) {
	rows, err := store.New(s.pool).ListPriceProposals(ctx, store.ListPriceProposalsParams{State: state, Class: class, MaxRows: int32(min(max(limit, 1), 1000))}) // #nosec G115 -- bounded
	if err != nil {
		return nil, err
	}
	out := make([]Proposal, 0, len(rows))
	for _, row := range rows {
		p, err := toProposal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Accepted is the result of accepting a proposal.
type Accepted struct {
	Version  PlatformVersion
	Warnings []string
}

// AcceptOptions are what a person adds when accepting a proposal.
type AcceptOptions struct {
	// Confirm is the model id typed out, for a proposal of class confirm.
	Confirm string
	// MaxOutput sets the entry's output limit in tokens; required when the source reports none
	// (pricefetch.ErrNoMaxOutput), and otherwise it replaces the proposed one. Zero keeps it.
	MaxOutput int
}

// Accept writes a proposal into a new platform version, from now on. A proposal of class confirm
// needs opts.Confirm to equal the model id exactly; a refused one cannot be accepted, and one
// without an output limit needs opts.MaxOutput. The proposal must still be diffed against the price
// in use.
func (r *Refresher) Accept(ctx context.Context, actor audit.Actor, id string, opts AcceptOptions) (Accepted, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return Accepted{}, &InvalidError{Msg: "proposal id " + id + " is not a UUID"}
	}
	if opts.MaxOutput < 0 {
		return Accepted{}, &InvalidError{Msg: fmt.Sprintf("--max-output %d is negative", opts.MaxOutput)}
	}
	var out Accepted
	_, err = audit.InTx(ctx, r.svc.pool, func(q *store.Queries, b audit.Batch) error {
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		row, err := q.GetPriceProposal(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: proposal %s", ErrNotFound, id)
		} else if err != nil {
			return err
		}
		p, err := toProposal(row)
		if err != nil {
			return err
		}
		switch {
		case p.State != StateOpen:
			return &InvalidError{Msg: "proposal " + id + " is " + p.State + ", not open"}
		case p.Class == pricefetch.ClassRefused || p.Proposed == nil:
			return &InvalidError{Msg: "proposal " + id + " is refused and cannot be accepted: " + p.FailedRules}
		case p.Class == pricefetch.ClassConfirm && opts.Confirm != p.Model:
			return &InvalidError{Msg: fmt.Sprintf("proposal %s needs a typed confirmation: pass the model id %q to accept it (%s)", id, p.Model, p.FailedRules)}
		}
		reason := "accepted proposal " + id + " for " + p.Model
		if opts.MaxOutput > 0 {
			p.Proposed.MaxOutput = opts.MaxOutput
			reason += fmt.Sprintf(", max_output %d set by the approver", opts.MaxOutput)
		}
		if p.Proposed.MaxOutput == 0 {
			return &InvalidError{Msg: fmt.Sprintf("proposal %s has no output limit: the source reports none for %s; pass --max-output <tokens>", id, p.Model)}
		}
		now := r.now()
		v, err := r.svc.view(ctx, q, now)
		if err != nil {
			return err
		}
		cur := v.effective(p.ProviderType, p.Model, now)
		switch {
		case (cur == nil) != (p.Current == nil), cur != nil && !sameSpec(cur.Entry.Spec, p.Current.Spec):
			return &InvalidError{Msg: "the price of " + p.Model + " changed since the proposal was made; fetch again and review the new proposal"}
		}
		a, err := r.applyEntries(ctx, q, v, []Proposal{p}, actor, now)
		if err != nil {
			return err
		}
		pv, err := writeVersion(ctx, q, b, versionWrite{
			Actor: actor, Action: "platform_price.set", Reason: reason,
			ProposalIDs: a.IDs, Entries: a.Entries, Changed: a.Changed, Dropped: a.Dropped,
		})
		if err != nil {
			return err
		}
		out = Accepted{Version: pv, Warnings: EntryWarnings(*p.Proposed)}
		return r.decide(ctx, q, b, p, StateAccepted, actor, pv.Version)
	})
	if err == nil {
		_ = r.metrics.refreshProposals(ctx, store.New(r.svc.pool))
	}
	return out, err
}

// Dismiss closes a proposal without applying it. The same values from the same source for the
// same model do not come back.
func (r *Refresher) Dismiss(ctx context.Context, actor audit.Actor, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return &InvalidError{Msg: "proposal id " + id + " is not a UUID"}
	}
	_, err = audit.InTx(ctx, r.svc.pool, func(q *store.Queries, b audit.Batch) error {
		row, err := q.GetPriceProposal(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: proposal %s", ErrNotFound, id)
		} else if err != nil {
			return err
		}
		p, err := toProposal(row)
		if err != nil {
			return err
		}
		if p.State != StateOpen {
			return &InvalidError{Msg: "proposal " + id + " is " + p.State + ", not open"}
		}
		return r.decide(ctx, q, b, p, StateDismissed, actor, 0)
	})
	if err == nil {
		_ = r.metrics.refreshProposals(ctx, store.New(r.svc.pool))
	}
	return err
}

// Import runs the proposal flow on snapshots from a file made by "prices fetch --out". The
// uploader vouches for the file: the install cannot re-check the body hash. Imported proposals are
// never applied automatically. Returns how many proposals were created or refreshed.
func (r *Refresher) Import(ctx context.Context, actor audit.Actor, f pricefetch.File) (int, error) {
	held, err := r.heldBounds(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	_, err = audit.InTx(ctx, r.svc.pool, func(q *store.Queries, b audit.Batch) error {
		now := r.now()
		if err := pricefetch.CheckFile(f, now); err != nil {
			return &InvalidError{Msg: err.Error()}
		}
		var uploaded []map[string]any
		for _, snap := range f.Snapshots {
			for _, o := range snap.Offers {
				if o.ProviderType != "openrouter" {
					return &InvalidError{Msg: fmt.Sprintf("the file prices %q models for provider type %q: only openrouter is supported", o.Model, o.ProviderType)}
				}
			}
			floor := 1
			if a, ok := r.adapters[snap.Source]; ok {
				floor = minModelsOf(a)
			} else if snap.Source == pricefetch.SourceOpenRouter {
				floor = pricefetch.OpenRouterMinModels
			}
			if err := pricefetch.CheckFloor(snap, floor, 0); err != nil {
				return &InvalidError{Msg: err.Error()}
			}
			// fetched_at keeps the file's time as provenance; the due time and the last-good floor
			// read only the install's own fetches, so neither the time nor the count moves them.
			if _, err := q.InsertPriceSourceFetch(ctx, store.InsertPriceSourceFetchParams{
				Source: snap.Source, Url: snap.URL, FetchedAt: pgtype.Timestamptz{Time: snap.FetchedAt, Valid: true},
				Result: FetchOK, Sha256: snap.SHA256, Etag: truncate(snap.ETag, 290), CommitSha: truncate(snap.CommitSHA, 60), ModelCount: int32(snap.Count), Trigger: TriggerImport, // #nosec G115 -- CheckFile bounds it to int32
			}); err != nil {
				return err
			}
			n, err := r.propose(ctx, q, snap, OriginImport, actor.String(), now, held)
			if err != nil {
				return err
			}
			total += n
			uploaded = append(uploaded, map[string]any{"source": snap.Source, "fetchedAt": snap.FetchedAt.Format(time.RFC3339), "sha256": snap.SHA256, "models": snap.Count, "proposals": n})
		}
		auditPlatform(b, actor, "price.import.uploaded", "price_import", "", nil, map[string]any{"snapshots": uploaded, "vouchedBy": actor.String()})
		return nil
	})
	if err == nil {
		_ = r.metrics.refreshProposals(ctx, store.New(r.svc.pool))
	}
	return total, err
}
