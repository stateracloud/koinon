// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
// auto-apply bounds. Fetching happens only when a caller asks (the CLI) or Loop runs, which koinon-api
// starts only when prices.refresh is enabled (ADR 0098 decision 2.2).
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
// the proposals inside the bounds. A failing source does not stop the others.
func (r *Refresher) Run(ctx context.Context, trigger string, auto bool) (RunResult, error) {
	res := RunResult{Failed: map[string]string{}}
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		n, err := r.fetchOne(ctx, r.adapters[name], trigger)
		if err != nil {
			res.Failed[name] = err.Error()
			r.log.Warn("price source fetch failed", "source", name, "err", err)
			continue
		}
		res.Fetched = append(res.Fetched, name)
		res.Proposals += n
	}
	if auto {
		n, err := r.AutoApply(ctx)
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

func (r *Refresher) fetchOne(ctx context.Context, a pricefetch.Adapter, trigger string) (int, error) {
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
		n, err = r.propose(ctx, tq, snap, OriginFetch, "", now)
		return err
	})
	if err != nil {
		return 0, err
	}
	r.metrics.fetched(a.Name(), FetchOK)
	return n, nil
}

// propose diffs a snapshot against the effective prices and stores the proposals. It returns how
// many proposals it created or refreshed. For a fetch it first records the observations that the
// stability rule reads.
func (r *Refresher) propose(ctx context.Context, q *store.Queries, snap pricefetch.Snapshot, origin, vouchedBy string, now time.Time) (int, error) {
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
			made, err := r.proposeModel(ctx, q, v, snap, o, origin, vouchedBy, now)
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

func (r *Refresher) proposeModel(ctx context.Context, q *store.Queries, v priceView, snap pricefetch.Snapshot, o pricefetch.Offer, origin, vouchedBy string, now time.Time) (bool, error) {
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
	if err != nil {
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

// AutoApply applies the open proposals of class auto in one platform version (actor auto), unless
// the day's cap would be exceeded, in which case none applies and they wait for a person. It
// returns how many it applied.
func (r *Refresher) AutoApply(ctx context.Context) (int, error) {
	applied := 0
	err := pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
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
			p, err := toProposal(row)
			if err != nil {
				return err
			}
			// Only a proposal still diffed against the price in use applies.
			if cur := v.effective(p.ProviderType, p.Model, now); cur == nil || p.Current == nil || !sameSpec(cur.Entry.Spec, p.Current.Spec) || p.Proposed == nil {
				continue
			}
			cands = append(cands, p)
		}
		if len(cands) == 0 {
			return nil
		}
		since := pgtype.Timestamptz{Time: now.Add(-24 * time.Hour), Valid: true}
		today, err := q.CountAutoAppliedPriceProposalsSince(ctx, since)
		if err != nil {
			return err
		}
		if !pricefetch.CapAllows(int(today), len(cands), r.bounds.MaxAutoPerDay) {
			why := fmt.Sprintf("daily cap: %d automatic change(s) in the last 24 hours and %d more waiting exceed the cap of %d; a large batch looks like a broken source, so all of them wait for a person", today, len(cands), r.bounds.MaxAutoPerDay)
			for _, p := range cands {
				id, _ := parseUUID(p.ID)
				if err := q.SetPriceProposalClass(ctx, store.SetPriceProposalClassParams{ID: id, Class: string(pricefetch.ClassNeedsReview), FailedRules: why}); err != nil {
					return err
				}
			}
			return nil
		}
		entries, ids, changed, err := r.applyEntries(ctx, q, v, cands, AutoActor, now)
		if err != nil {
			return err
		}
		pv, err := writeVersion(ctx, q, versionWrite{
			Actor: AutoActor, Action: "platform_price.set", Reason: fmt.Sprintf("%d automatic price update(s) within bounds", len(cands)),
			RuleSet: pricefetch.RuleSetVersion, ProposalIDs: ids, Entries: entries, Changed: changed,
		})
		if err != nil {
			return err
		}
		for _, p := range cands {
			if err := r.decide(ctx, q, p, StateAccepted, AutoActor, pv.Version); err != nil {
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
// sets the reviewed price to the proposed one; an automatic one keeps the last reviewed price.
func (r *Refresher) applyEntries(ctx context.Context, q *store.Queries, v priceView, ps []Proposal, actor audit.Actor, now time.Time) ([]PlatformEntry, []pgtype.UUID, []string, error) {
	cur, err := currentPlatform(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := map[string]PlatformEntry{}
	for _, e := range cur.Entries {
		byID[e.Entry.ID] = e
	}
	var ids []pgtype.UUID
	var changed []string
	for _, p := range ps {
		if p.Proposed == nil {
			return nil, nil, nil, &InvalidError{Msg: "proposal " + p.ID + " has no entry"}
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
		prov.ProposalID, prov.Approver, prov.ApprovedAt = p.ID, actorText(actor), now.UTC()
		if actor == AutoActor {
			prov.RuleSetVersion = pricefetch.RuleSetVersion
		}
		byID[e.ID] = PlatformEntry{Entry: e, Reviewed: reviewed, Provenance: prov}
		changed = append(changed, e.ID)
		id, err := parseUUID(p.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		ids = append(ids, id)
	}
	out := make([]PlatformEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	return out, ids, changed, nil
}

// decide marks a proposal accepted or dismissed and audits it. A proposal already decided is an
// InvalidError.
func (r *Refresher) decide(ctx context.Context, q *store.Queries, p Proposal, state string, actor audit.Actor, version int64) error {
	id, err := parseUUID(p.ID)
	if err != nil {
		return err
	}
	params := store.DecidePriceProposalParams{ID: id, State: state, DecidedBy: actorText(actor)}
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
	return auditPlatform(ctx, q, actor, action, "price_proposal", p.ID, nil, after)
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

// Accept writes a proposal into a new platform version, from now on. A proposal of class confirm
// needs confirmModel to equal the model id exactly; a refused one cannot be accepted. The
// proposal must still be diffed against the price in use.
func (r *Refresher) Accept(ctx context.Context, actor audit.Actor, id, confirmModel string) (Accepted, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return Accepted{}, &InvalidError{Msg: "proposal id " + id + " is not a UUID"}
	}
	var out Accepted
	err = pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
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
		case p.Class == pricefetch.ClassConfirm && confirmModel != p.Model:
			return &InvalidError{Msg: fmt.Sprintf("proposal %s needs a typed confirmation: pass the model id %q to accept it (%s)", id, p.Model, p.FailedRules)}
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
		entries, ids, changed, err := r.applyEntries(ctx, q, v, []Proposal{p}, actor, now)
		if err != nil {
			return err
		}
		pv, err := writeVersion(ctx, q, versionWrite{
			Actor: actor, Action: "platform_price.set", Reason: "accepted proposal " + id + " for " + p.Model,
			ProposalIDs: ids, Entries: entries, Changed: changed,
		})
		if err != nil {
			return err
		}
		out = Accepted{Version: pv, Warnings: EntryWarnings(*p.Proposed)}
		return r.decide(ctx, q, p, StateAccepted, actor, pv.Version)
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
	err = pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
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
		return r.decide(ctx, q, p, StateDismissed, actor, 0)
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
	total := 0
	err := pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		now := r.now()
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
			if _, err := q.InsertPriceSourceFetch(ctx, store.InsertPriceSourceFetchParams{
				Source: snap.Source, Url: snap.URL, FetchedAt: pgtype.Timestamptz{Time: snap.FetchedAt, Valid: !snap.FetchedAt.IsZero()},
				Result: FetchOK, Sha256: snap.SHA256, Etag: truncate(snap.ETag, 290), CommitSha: truncate(snap.CommitSHA, 60), ModelCount: int32(snap.Count), Trigger: TriggerImport, // #nosec G115 -- bounded by the file cap
			}); err != nil {
				return err
			}
			n, err := r.propose(ctx, q, snap, OriginImport, actorText(actor), now)
			if err != nil {
				return err
			}
			total += n
			uploaded = append(uploaded, map[string]any{"source": snap.Source, "fetchedAt": snap.FetchedAt.Format(time.RFC3339), "sha256": snap.SHA256, "models": snap.Count, "proposals": n})
		}
		return auditPlatform(ctx, q, actor, "price.import.uploaded", "price_import", "", nil, map[string]any{"snapshots": uploaded, "vouchedBy": actorText(actor)})
	})
	if err == nil {
		_ = r.metrics.refreshProposals(ctx, store.New(r.svc.pool))
	}
	return total, err
}

// SyncSettings records the refresh setting koinon-api started with and audits a change
// (price.refresh.settings_changed). The first start with the defaults (off) records and audits
// nothing. A bound looser than the default never gets here: NewRefresher refuses it.
func (r *Refresher) SyncSettings(ctx context.Context, enabled bool, interval time.Duration) error {
	return pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		want := store.UpsertPriceRefreshSettingsParams{
			Enabled: enabled, Interval: interval.String(), MaxChangePercent: int32(r.bounds.MaxChangePercent), MaxAutoPerDay: int32(r.bounds.MaxAutoPerDay), // #nosec G115 -- validated bounds
		}
		old, err := q.GetPriceRefreshSettings(ctx)
		first := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !first {
			return err
		}
		if !first && old.Enabled == want.Enabled && old.Interval == want.Interval && old.MaxChangePercent == want.MaxChangePercent && old.MaxAutoPerDay == want.MaxAutoPerDay {
			return nil
		}
		if err := q.UpsertPriceRefreshSettings(ctx, want); err != nil {
			return err
		}
		if first && !enabled {
			return nil
		}
		fmtSettings := func(en bool, iv string, pct, perDay int32) map[string]any {
			return map[string]any{"enabled": en, "interval": iv, "maxChangePercent": pct, "maxAutoPerDay": perDay}
		}
		var before any
		if !first {
			before = fmtSettings(old.Enabled, old.Interval, old.MaxChangePercent, old.MaxAutoPerDay)
		}
		return auditPlatform(ctx, q, audit.System(), "price.refresh.settings_changed", "price_refresh", "", before,
			fmtSettings(want.Enabled, want.Interval, want.MaxChangePercent, want.MaxAutoPerDay))
	})
}

// Loop fetches on a schedule until ctx ends and applies the proposals inside the bounds. A source
// fetched less than interval ago (another replica did, or this one before a restart) is not
// fetched again. koinon-api calls Loop only when prices.refresh is enabled.
func (r *Refresher) Loop(ctx context.Context, interval time.Duration) {
	tick := time.NewTimer(time.Minute) // a restart does not fetch at once
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if r.due(ctx, interval) {
			if _, err := r.Run(ctx, TriggerSchedule, true); err != nil && ctx.Err() == nil {
				r.log.Warn("scheduled price refresh", "err", err)
			}
		}
		tick.Reset(min(interval, time.Hour))
	}
}

// due says whether some adapter was last fetched at least interval ago.
func (r *Refresher) due(ctx context.Context, interval time.Duration) bool {
	q := store.New(r.svc.pool)
	for name := range r.adapters {
		f, err := q.GetLastPriceSourceFetch(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.now().Sub(f.FetchedAt.Time) >= interval) {
			return true
		}
	}
	return false
}
