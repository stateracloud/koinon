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

// AutoActor is the actor of the price refresh and its bounded auto-apply: system
// koinon-api:price-refresh (ADR 0100 decision 2). The platform price tables' actor columns store the
// same text the install audit log shows, Actor.String().
var AutoActor = audit.System("koinon-api:price-refresh")

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
	// Approver is the actor who accepted the proposal (Actor.String(): "system koinon-api:price-refresh", "user <id>", "cli <command>").
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
	// BadEntries counts the stored entries that no longer decode or validate. They are left out of
	// Entries (and of a version written from this one) and the resolver skips them.
	BadEntries int `json:"badEntries,omitempty"`
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
	rs, badN := resolvePlatformRows(v, rows)
	// Parsed once per version and process, so the warning and the gauge follow the version in use.
	platformBadEntries.Set(float64(badN))
	if badN > 0 {
		slog.Default().Warn("platform price entries that no longer decode or validate are skipped; their models fall through to the default price list",
			"version", v.Version, "entries", badN)
	}
	platformParsed.Store(key, rs)
	return v.Version, rs, nil
}

// resolvePlatformRows resolves a version's stored entries. An entry that no longer decodes or
// validates (a later binary's stricter rules, a hand-edited row) is skipped and counted, not an
// error: one bad entry must not fail every request priced without an org entry. Its models fall
// through to the default list.
func resolvePlatformRows(v store.PlatformPriceVersion, rows []store.PlatformPriceEntry) ([]pricing.Resolved, int) {
	rs := make([]pricing.Resolved, 0, len(rows))
	badN := 0
	for _, row := range rows {
		var e pricing.Entry
		if err := json.Unmarshal(row.Entry, &e); err != nil {
			badN++
			continue
		}
		e.ID = row.EntryID
		r, err := e.Resolve(true)
		if err != nil {
			badN++
			continue
		}
		// The version, not the entry's date, says when the price starts.
		r.ValidFrom = v.ValidFrom.Time
		rs = append(rs, r)
	}
	return rs, badN
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
		if json.Unmarshal(r.Entry, &pe.Entry) != nil || json.Unmarshal(r.Reviewed, &pe.Reviewed) != nil ||
			json.Unmarshal(r.Provenance, &pe.Provenance) != nil {
			out.BadEntries++
			continue
		}
		pe.Entry.ID = r.EntryID
		if _, err := pe.Entry.Resolve(true); err != nil {
			out.BadEntries++
			continue
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
	// Dropped counts the entries of the version written from that no longer decode or validate and
	// are not carried over; the audit event says how many.
	Dropped int
}

// writeVersion validates entries, writes the next version and audits it in the same
// transaction as q. The caller holds the platform lock (LockPlatformPrices).
func writeVersion(ctx context.Context, q *store.Queries, b audit.Batch, w versionWrite) (PlatformVersion, error) {
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
		Actor: w.Actor.String(), Reason: w.Reason, RuleSetVersion: w.RuleSet, ProposalIds: w.ProposalIDs,
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
	if w.Dropped > 0 {
		after["droppedBadEntries"] = w.Dropped
	}
	if len(pv.ProposalIDs) > 0 {
		after["proposals"] = pv.ProposalIDs
	}
	auditPlatform(b, w.Actor, w.Action, "platform_price", fmt.Sprint(row.Version), nil, after)
	return pv, nil
}

// auditPlatform records a platform-wide event in the install audit chain (ADR 0100). It belongs to
// no org and needs no platform admin: a change before the first admin exists is one install event
// with a named actor, a user, a cli command or system koinon-api:price-refresh. It is written in the
// transaction of the change (the audit batch writes it just before commit, after the platform price
// lock the caller holds, ADR 0100 decision 1.7), and a failed write fails the change.
func auditPlatform(b audit.Batch, actor audit.Actor, action, kind, target string, before, after any) {
	b.Install(audit.InstallEvent{Actor: actor, Action: action, TargetKind: kind, TargetID: target, Before: before, After: after})
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
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
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
		out, err = writeVersion(ctx, q, b, versionWrite{
			Actor: actor, Action: "platform_price.rollback", Reason: fmt.Sprintf("rollback to version %d", version),
			RestoredFrom: version, Entries: old.Entries, Changed: changedIDs(cur.Entries, old.Entries), Dropped: old.BadEntries,
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
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		if err := q.LockPlatformPrices(ctx); err != nil {
			return err
		}
		cur, err := currentPlatform(ctx, q)
		if err != nil {
			return err
		}
		if cur.BadEntries > 0 {
			// Retiring would write a version without the bad entries, a change no person made and the
			// audit event would not show. Leave the version as it is; a person rolls back or replaces it.
			s.log.Warn("platform price retirement skipped: the current platform version has entries that no longer decode or validate",
				"version", cur.Version, "entries", cur.BadEntries)
			return nil
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
		_, err = writeVersion(ctx, q, b, versionWrite{
			Actor: AutoActor, Action: "platform_price.retired", Reason: "default price list " + list.Version + " caught up",
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
