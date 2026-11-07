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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/pricing"
)

// PlatformRef names a platform entry in a ledger row: "platform@<version>#<id>" (ADR 0098 decision 1.3).
func PlatformRef(version int64, id string) string { return fmt.Sprintf("platform@%d#%s", version, id) }

// AutoActor is the actor of the bounded auto-apply. Its text in a version's actor column is "auto".
var AutoActor = audit.Actor{Kind: audit.KindSystem, ID: "auto"}

func actorText(a audit.Actor) string {
	if a == AutoActor {
		return "auto"
	}
	return a.String()
}

// Provenance is where a platform price came from (ADR 0098 decision 4).
type Provenance struct {
	// Source is the source id ("openrouter") or "manual" for a price a person typed.
	Source          string    `json:"source"`
	URL             string    `json:"url,omitempty"`
	FetchedAt       time.Time `json:"fetchedAt,omitzero"`
	SHA256          string    `json:"sha256,omitempty"`
	CommitSHA       string    `json:"commitSha,omitempty"`
	ETag            string    `json:"etag,omitempty"`
	Licence         string    `json:"licence,omitempty"`
	AgreeingSources []string  `json:"agreeingSources,omitempty"`
	ProposalID      string    `json:"proposalId,omitempty"`
	// Approver is "auto" or the actor who accepted the proposal ("user <id>", "cli <command>").
	Approver       string    `json:"approver"`
	RuleSetVersion string    `json:"ruleSetVersion,omitempty"`
	ApprovedAt     time.Time `json:"approvedAt,omitzero"`
	// VouchedBy is the uploader of an imported file: the install cannot re-check the body hash.
	VouchedBy string `json:"vouchedBy,omitempty"`
	// Ignored is the source's data Koinon cannot bill, kept for reference only.
	Ignored map[string]string `json:"ignored,omitempty"`
}

// PlatformEntry is one entry of a platform version with its provenance.
type PlatformEntry struct {
	Entry pricing.Entry `json:"entry"`
	// Reviewed is the price a person last reviewed for this entry. An auto-applied version keeps it,
	// so the bound on automatic moves is measured against it and cannot compound.
	Reviewed   pricing.Spec `json:"reviewed"`
	Provenance Provenance   `json:"provenance"`
}

// PlatformVersion is a version of the platform price list with its entries.
type PlatformVersion struct {
	Version        int64           `json:"version"`
	CreatedAt      time.Time       `json:"createdAt"`
	ValidFrom      time.Time       `json:"validFrom"`
	Actor          string          `json:"actor"`
	Reason         string          `json:"reason"`
	RuleSetVersion string          `json:"ruleSetVersion,omitempty"`
	ProposalIDs    []string        `json:"proposalIds,omitempty"`
	RestoredFrom   int64           `json:"restoredFrom,omitempty"`
	Entries        []PlatformEntry `json:"entries"`
}

type platformKey struct {
	version int64
	created time.Time
}

// platformParsed caches the parsed entries of platform versions. A version never changes once
// written, so (version, created_at) identifies its entries; the cache holds one slice per version
// this process has priced from, a handful over an install's life.
var platformParsed sync.Map

// platformEntries finds the platform version valid at t and its resolved entries. Version 0 and no
// entries: none was written yet.
func platformEntries(ctx context.Context, q *store.Queries, t time.Time) (int64, []pricing.Resolved, error) {
	v, err := q.GetPlatformPriceVersionAt(ctx, pgtype.Timestamptz{Time: t, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	} else if err != nil {
		return 0, nil, err
	}
	key := platformKey{v.Version, v.CreatedAt.Time}
	if c, ok := platformParsed.Load(key); ok {
		return v.Version, c.([]pricing.Resolved), nil
	}
	rows, err := q.ListPlatformPriceEntries(ctx, v.Version)
	if err != nil {
		return 0, nil, err
	}
	rs := make([]pricing.Resolved, 0, len(rows))
	for _, row := range rows {
		var e pricing.Entry
		if err := json.Unmarshal(row.Entry, &e); err != nil {
			return 0, nil, bad(fmt.Errorf("platform price %d#%s: %w", v.Version, row.EntryID, err))
		}
		e.ID = row.EntryID
		r, err := e.Resolve(true)
		if err != nil {
			return 0, nil, bad(err)
		}
		// The version, not the entry's date, says when the price starts.
		r.ValidFrom = v.ValidFrom.Time
		rs = append(rs, r)
	}
	platformParsed.Store(key, rs)
	return v.Version, rs, nil
}

func toPlatformVersion(v store.PlatformPriceVersion, rows []store.PlatformPriceEntry) (PlatformVersion, error) {
	out := PlatformVersion{
		Version: v.Version, CreatedAt: v.CreatedAt.Time, ValidFrom: v.ValidFrom.Time, Actor: v.Actor, Reason: v.Reason,
		RuleSetVersion: v.RuleSetVersion, RestoredFrom: v.RestoredFrom.Int64,
	}
	for _, id := range v.ProposalIds {
		out.ProposalIDs = append(out.ProposalIDs, id.String())
	}
	for _, r := range rows {
		pe := PlatformEntry{}
		if err := json.Unmarshal(r.Entry, &pe.Entry); err != nil {
			return PlatformVersion{}, err
		}
		pe.Entry.ID = r.EntryID
		if err := json.Unmarshal(r.Reviewed, &pe.Reviewed); err != nil {
			return PlatformVersion{}, err
		}
		if err := json.Unmarshal(r.Provenance, &pe.Provenance); err != nil {
			return PlatformVersion{}, err
		}
		out.Entries = append(out.Entries, pe)
	}
	return out, nil
}

// PlatformVersionAt returns the platform version valid at t. ok is false when none was written.
func (s *Service) PlatformVersionAt(ctx context.Context, t time.Time) (PlatformVersion, bool, error) {
	return platformVersionAt(ctx, store.New(s.pool), t)
}

func platformVersionAt(ctx context.Context, q *store.Queries, t time.Time) (PlatformVersion, bool, error) {
	v, err := q.GetPlatformPriceVersionAt(ctx, pgtype.Timestamptz{Time: t, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformVersion{}, false, nil
	} else if err != nil {
		return PlatformVersion{}, false, err
	}
	rows, err := q.ListPlatformPriceEntries(ctx, v.Version)
	if err != nil {
		return PlatformVersion{}, false, err
	}
	pv, err := toPlatformVersion(v, rows)
	return pv, err == nil, err
}

// PlatformVersions lists the newest versions without their entries.
func (s *Service) PlatformVersions(ctx context.Context, limit int) ([]PlatformVersion, error) {
	rows, err := store.New(s.pool).ListPlatformPriceVersions(ctx, int32(min(max(limit, 1), 500))) // #nosec G115 -- bounded above
	if err != nil {
		return nil, err
	}
	out := make([]PlatformVersion, 0, len(rows))
	for _, r := range rows {
		pv, err := toPlatformVersion(r, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, pv)
	}
	return out, nil
}

// versionWrite is one new platform version.
type versionWrite struct {
	// Log gets the warning when a system change finds no platform admin to audit in (auditPlatform);
	// nil means slog.Default.
	Log          *slog.Logger
	Actor        audit.Actor
	Action       string // platform_price.set, platform_price.rollback or platform_price.retired
	Reason       string
	RuleSet      string
	ProposalIDs  []pgtype.UUID
	RestoredFrom int64
	// ValidFrom zero means now. A time in the past is refused: backdating would change what a
	// charged request cost.
	ValidFrom time.Time
	Entries   []PlatformEntry
	// Changed lists the entry ids the version adds, changes or removes, for the audit event.
	Changed []string
}

// writeVersion validates entries, writes the next version and audits it in the same
// transaction as q. The caller holds the platform lock (LockPlatformPrices).
func writeVersion(ctx context.Context, q *store.Queries, w versionWrite) (PlatformVersion, error) {
	if !w.ValidFrom.IsZero() && w.ValidFrom.Before(time.Now().Add(-time.Second)) {
		return PlatformVersion{}, &InvalidError{Msg: "a platform price version cannot start in the past: backdating would change what charged requests cost"}
	}
	seen := map[string]bool{}
	for _, pe := range w.Entries {
		if seen[pe.Entry.ID] {
			return PlatformVersion{}, &InvalidError{Msg: "platform price entry " + pe.Entry.ID + " appears twice"}
		}
		seen[pe.Entry.ID] = true
		if _, err := pe.Entry.Resolve(true); err != nil {
			return PlatformVersion{}, &InvalidError{Msg: err.Error()}
		}
	}
	vw := store.InsertPlatformPriceVersionParams{
		Actor: actorText(w.Actor), Reason: w.Reason, RuleSetVersion: w.RuleSet, ProposalIds: w.ProposalIDs,
	}
	if vw.ProposalIds == nil {
		vw.ProposalIds = []pgtype.UUID{}
	}
	if !w.ValidFrom.IsZero() {
		vw.ValidFrom = pgtype.Timestamptz{Time: w.ValidFrom, Valid: true}
	}
	if w.RestoredFrom > 0 {
		vw.RestoredFrom = pgtype.Int8{Int64: w.RestoredFrom, Valid: true}
	}
	row, err := q.InsertPlatformPriceVersion(ctx, vw)
	if err != nil {
		return PlatformVersion{}, err
	}
	entries := slices.Clone(w.Entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Entry.ID < entries[j].Entry.ID })
	for _, pe := range entries {
		eb, err := json.Marshal(pe.Entry)
		if err != nil {
			return PlatformVersion{}, err
		}
		rb, err := json.Marshal(pe.Reviewed)
		if err != nil {
			return PlatformVersion{}, err
		}
		pb, err := json.Marshal(pe.Provenance)
		if err != nil {
			return PlatformVersion{}, err
		}
		if err := q.InsertPlatformPriceEntry(ctx, store.InsertPlatformPriceEntryParams{Version: row.Version, EntryID: pe.Entry.ID, Entry: eb, Reviewed: rb, Provenance: pb}); err != nil {
			return PlatformVersion{}, err
		}
	}
	pv, err := toPlatformVersion(row, nil)
	if err != nil {
		return PlatformVersion{}, err
	}
	pv.Entries = entries
	after := map[string]any{"version": row.Version, "reason": w.Reason, "entries": len(entries), "changed": w.Changed, "validFrom": row.ValidFrom.Time.Format(time.RFC3339)}
	if w.RuleSet != "" {
		after["ruleSetVersion"] = w.RuleSet
	}
	if w.RestoredFrom > 0 {
		after["restoredFrom"] = w.RestoredFrom
	}
	if len(pv.ProposalIDs) > 0 {
		after["proposals"] = pv.ProposalIDs
	}
	if err := auditPlatform(ctx, q, w.Log, w.Actor, w.Action, "platform_price", fmt.Sprint(row.Version), nil, after); err != nil {
		return PlatformVersion{}, err
	}
	return pv, nil
}

// auditPlatform records a platform-wide event. It belongs to no org, so, as the platform admin API
// audits in the acting admin's personal org, it is written in the personal org of every platform
// admin (the same rule as keys forget): the change acts for none of them.
//
// Without a platform admin (a fresh install: admins are granted at their first login) there is no
// org to audit in. A person's change (a CLI actor: accept, dismiss, rollback, import) is then
// refused, so that no decision a person made goes unaudited. Koinon's own bookkeeping (a system
// actor: the settings sync, retirement when a release caught up, the bounded auto-apply) must not
// depend on an admin existing, or koinon-api could not start on a fresh install: it goes ahead,
// and log gets a warning. Its record stays in the install-wide tables it writes, which are
// append-only and carry the actor, the reason and the rule set (platform_price_versions,
// price_proposals.decided_by, price_refresh_settings).
func auditPlatform(ctx context.Context, q *store.Queries, log *slog.Logger, actor audit.Actor, action, kind, target string, before, after any) error {
	orgs, err := q.ListPlatformAdminPersonalOrgs(ctx)
	if err != nil {
		return err
	}
	if len(orgs) == 0 {
		if actor.Kind == audit.KindSystem {
			if log == nil {
				log = slog.Default()
			}
			log.Warn("no platform admin exists yet, so this platform price change is in no org's audit log; the platform price tables record it",
				"action", action, "actor", actorText(actor))
			return nil
		}
		return errors.New("prices: no platform admin exists to audit the change in; grant one first")
	}
	for _, org := range orgs {
		if err := audit.RecordErr(ctx, q, audit.Event{Org: org, Actor: actor, Action: action, TargetKind: kind, TargetID: target, Before: before, After: after}); err != nil {
			return err
		}
	}
	return nil
}

// currentPlatform returns the version valid now (zero value, no entries, when none).
func currentPlatform(ctx context.Context, q *store.Queries) (PlatformVersion, error) {
	pv, _, err := platformVersionAt(ctx, q, time.Now())
	return pv, err
}

// Rollback writes a new version that restores the entries of an earlier one from now on. History
// is never rewritten, and requests charged in between keep their price_ref (ADR 0098 decision 5).
func (s *Service) Rollback(ctx context.Context, actor audit.Actor, version int64) (PlatformVersion, error) {
	var out PlatformVersion
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		v, err := q.GetPlatformPriceVersion(ctx, version)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: platform price version %d", ErrNotFound, version)
		} else if err != nil {
			return err
		}
		rows, err := q.ListPlatformPriceEntries(ctx, version)
		if err != nil {
			return err
		}
		old, err := toPlatformVersion(v, rows)
		if err != nil {
			return err
		}
		cur, err := currentPlatform(ctx, q)
		if err != nil {
			return err
		}
		out, err = writeVersion(ctx, q, versionWrite{
			Log: s.log, Actor: actor, Action: "platform_price.rollback", Reason: fmt.Sprintf("rollback to version %d", version),
			RestoredFrom: version, Entries: old.Entries, Changed: changedIDs(cur.Entries, old.Entries),
		})
		return err
	})
	return out, err
}

// changedIDs lists the ids whose entry differs between two sets, or is in only one.
func changedIDs(before, after []PlatformEntry) []string {
	b := map[string]string{}
	for _, e := range before {
		j, _ := json.Marshal(e.Entry.Spec)
		b[e.Entry.ID] = string(j) + fmt.Sprint(e.Entry.MaxOutput)
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, e := range after {
		j, _ := json.Marshal(e.Entry.Spec)
		seen[e.Entry.ID] = true
		if b[e.Entry.ID] != string(j)+fmt.Sprint(e.Entry.MaxOutput) {
			ids = append(ids, e.Entry.ID)
		}
	}
	for id := range b {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// RetireCaughtUp removes the platform entries that the current default list now equals (ADR 0098
// decision 1.5): an entry whose rates, tiers and limits equal the default entry of the same id,
// valid today. It writes one new version without them, audited platform_price.retired with the
// default list's version as the reason, and returns the retired ids. It is idempotent: koinon-api
// runs it at every start.
func (s *Service) RetireCaughtUp(ctx context.Context) ([]string, error) {
	list, defs := s.src.Current()
	today := time.Now().UTC()
	latest := map[string]pricing.Resolved{}
	for _, d := range defs {
		if d.ValidFrom.After(today) {
			continue
		}
		if p, ok := latest[d.Entry.ID]; !ok || d.ValidFrom.After(p.ValidFrom) {
			latest[d.Entry.ID] = d
		}
	}
	var retired []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		cur, err := currentPlatform(ctx, q)
		if err != nil {
			return err
		}
		var keep []PlatformEntry
		for _, pe := range cur.Entries {
			if d, ok := latest[pe.Entry.ID]; ok && sameLimitsAndRates(pe.Entry, d.Entry) {
				retired = append(retired, pe.Entry.ID)
				continue
			}
			keep = append(keep, pe)
		}
		if len(retired) == 0 {
			return nil
		}
		_, err = writeVersion(ctx, q, versionWrite{
			Log: s.log, Actor: audit.System(), Action: "platform_price.retired", Reason: "default price list " + list.Version + " caught up",
			Entries: keep, Changed: retired,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(retired) > 0 {
		s.log.Info("platform price entries retired: the default list caught up", "list", list.Version, "entries", retired)
	}
	return retired, nil
}

// sameLimitsAndRates compares what a price charges and caps: rates, tiers and the output limit.
func sameLimitsAndRates(a, b pricing.Entry) bool {
	ja, _ := json.Marshal(a.Spec)
	jb, _ := json.Marshal(b.Spec)
	return string(ja) == string(jb) && a.MaxOutput == b.MaxOutput
}
