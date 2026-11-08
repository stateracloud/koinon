// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/audit/internal/auditstore"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Install audit anchors (ADR 0100 decision 4.1). On a schedule one audit.anchor event is written to
// the install chain; in the same transaction the heads of every org chain are inserted into the
// append-only side table install_audit_anchor_heads, and the event's summary holds org_count and
// heads_root, a hash over those heads. The install head then commits to every org head, so a rewrite
// or rollback of an org chain behind an anchor shows (VerifyAnchors, and VerifyExport with the anchors
// of an install export), as long as the install chain itself is intact. Anchors alone do not stop
// someone who can rewrite the install chain too; signed checkpoints (issue 7) close that gap.

const (
	// ActionAnchor is the action of an anchor event in the install chain.
	ActionAnchor = "audit.anchor"
	// AnchorActorID is the system actor that writes anchors (ADR 0100 decision 2).
	AnchorActorID = "koinon-api:audit-anchor"

	// anchorRootTag domain-separates heads_root from the chain hashes.
	anchorRootTag = "koinon.install_audit.anchor.v1"
)

// AnchorHead is one org chain head an anchor commits to.
type AnchorHead struct {
	// OrgID is the org's ID as lower-case UUID text.
	OrgID    string
	ChainSeq int64
	// Hash is the 32-byte hash of the org chain's row at ChainSeq.
	Hash []byte
}

// HeadsRoot is the heads_root of an anchor: sha256 over the tag, the install ID, the number of heads
// and then the heads in org ID order, each (org_id, chain_seq, hash) with every field length-prefixed
// as in the chain encoding (ADR 0100 decision 1.3): org_id as lower-case UUID text, chain_seq in
// decimal and hash as lower-case hex. The install ID and the tag bind the root to this install.
// heads may be in any order; they are sorted by org ID first.
func HeadsRoot(installID string, heads []AnchorHead) []byte {
	sorted := append([]AnchorHead(nil), heads...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OrgID < sorted[j].OrgID })
	b := appendField(nil, anchorRootTag)
	b = appendField(b, installID)
	b = appendField(b, strconv.Itoa(len(sorted)))
	for _, h := range sorted {
		b = appendField(b, h.OrgID)
		b = appendField(b, strconv.FormatInt(h.ChainSeq, 10))
		b = appendField(b, hex.EncodeToString(h.Hash))
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

// anchorAfter is the "after" part of an anchor event's summary.
type anchorAfter struct {
	OrgCount  int    `json:"org_count"`
	HeadsRoot string `json:"heads_root"`
}

// parseAnchorSummary reads org_count and heads_root from an anchor event's summary text.
func parseAnchorSummary(text string) (anchorAfter, error) {
	var s struct {
		After *anchorAfter `json:"after"`
	}
	if err := json.Unmarshal([]byte(text), &s); err != nil || s.After == nil {
		return anchorAfter{}, errors.New("the anchor event has no org_count and heads_root in its summary")
	}
	return *s.After, nil
}

// AnchorOptions tunes Anchor.
type AnchorOptions struct {
	// MinAge skips the anchor when the newest one is younger than this (by the database's clock). It is
	// what keeps several replicas, or a restart, from writing two anchors for one tick. Zero always anchors.
	MinAge time.Duration
	// Wait waits for another anchor in progress instead of skipping (an anchor made on demand).
	Wait bool
}

// AnchorResult says what Anchor did.
type AnchorResult struct {
	// Written is true when an anchor event was written. Otherwise Skipped says why: "busy" (another
	// transaction is anchoring) or "recent" (the newest anchor is younger than MinAge).
	Written bool
	Skipped string
	// EventID, ChainSeq, OrgCount and HeadsRoot (hex) describe the anchor written. InstallHead is the
	// hash (hex) of the anchor event, the install chain's head after it.
	EventID     string
	ChainSeq    int64
	OrgCount    int
	HeadsRoot   string
	InstallHead string
	// Inconsistent lists what the consistency check found before the write (see Anchor); empty when the
	// org chains agree with the last anchor. The anchor is written either way.
	Inconsistent []AnchorInconsistency
}

// The reasons of an AnchorInconsistency, and the reason label values of koinon_audit_anchor_inconsistent_total.
const (
	// InconsistentHashChanged: the org chain has another hash (or no row) at the chain_seq the last anchor names.
	InconsistentHashChanged = "hash_changed"
	// InconsistentSeqRegressed: the org's chain head is below the chain_seq the last anchor names, or gone.
	InconsistentSeqRegressed = "seq_regressed"
)

// AnchorInconsistency is one org whose chain disagrees with the newest earlier anchor.
type AnchorInconsistency struct {
	// Org is the org's ID as lower-case UUID text (logs may name it; the metric never does, ADR 0023).
	Org string
	// Reason is InconsistentHashChanged or InconsistentSeqRegressed.
	Reason string
	// AnchoredSeq is the chain_seq the earlier anchor names for the org.
	AnchoredSeq int64
}

// checkConsistency compares the org chains with the newest earlier anchor before a new one is written,
// the rule of C2SP tlog-checkpoint that a signer never signs a checkpoint inconsistent with one it signed
// before. Per org it finds a hash other than the anchored one at the anchored chain_seq, and a head
// below the anchored chain_seq. Only the newest anchor is checked, not every earlier one: each earlier
// anchor was checked against the one before it when it was written, and VerifyAnchors checks the whole
// history. This keeps the cost at one anchor's heads (one row per org) per tick, however old the install is.
func checkConsistency(ctx context.Context, q *store.Queries, lastID string, now []AnchorHead) ([]AnchorInconsistency, error) {
	var id pgtype.UUID
	if err := id.Scan(lastID); err != nil {
		return nil, fmt.Errorf("anchor id %q: %w", lastID, err)
	}
	prev, err := q.CheckAnchoredHeads(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read the heads of the newest anchor: %w", err)
	}
	cur := make(map[string]int64, len(now))
	for _, h := range now {
		cur[h.OrgID] = h.ChainSeq
	}
	var out []AnchorInconsistency
	for _, r := range prev {
		if r.OrgHash == nil || !bytes.Equal(r.OrgHash, r.Hash) {
			out = append(out, AnchorInconsistency{Org: r.OrgText, Reason: InconsistentHashChanged, AnchoredSeq: r.ChainSeq})
		}
		if seq, ok := cur[r.OrgText]; !ok || seq < r.ChainSeq {
			out = append(out, AnchorInconsistency{Org: r.OrgText, Reason: InconsistentSeqRegressed, AnchoredSeq: r.ChainSeq})
		}
	}
	return out, nil
}

// Anchor writes one audit.anchor install event and the heads it commits to, in one transaction. The
// domain lock comes first (one anchor at a time across replicas), then the org heads are read as a plain
// read of committed rows, so no anchor names a head that is not committed (an org write in flight holds
// its head row and is not seen), and the install head is taken last by the event insert (ADR 0100
// decision 1.7). A database that refuses the insert for lack of privilege returns ErrInstallWriteDenied.
//
// Before it writes, under the lock, Anchor checks the org chains against the newest earlier anchor
// (checkConsistency). A mismatch is returned in AnchorResult.Inconsistent and counted in
// koinon_audit_anchor_inconsistent_total, and the anchor is still written, so the chain of anchors goes on
// and verify reports the break.
func Anchor(ctx context.Context, db Beginner, opt AnchorOptions) (AnchorResult, error) {
	var res AnchorResult
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		res = AnchorResult{}
		q := store.New(tx)
		var lastID string
		if opt.Wait {
			if err := q.LockAuditAnchor(ctx); err != nil {
				return fmt.Errorf("audit anchor: lock: %w", err)
			}
		} else {
			locked, err := q.TryLockAuditAnchor(ctx)
			if err != nil {
				return fmt.Errorf("audit anchor: lock: %w", err)
			}
			if !locked {
				res.Skipped = "busy"
				return nil
			}
		}
		last, err := q.GetLastInstallAnchor(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("audit anchor: read the newest anchor: %w", err)
		case opt.MinAge > 0 && last.AgeSeconds < opt.MinAge.Seconds():
			res.Skipped = "recent"
			return nil
		default:
			lastID = last.IDText
		}
		installID, err := q.GetInstallID(ctx)
		if err != nil {
			return fmt.Errorf("audit anchor: read the install ID: %w", err)
		}
		rows, err := q.ListAuditChainHeads(ctx)
		if err != nil {
			return fmt.Errorf("audit anchor: read the org chain heads: %w", err)
		}
		heads := make([]AnchorHead, len(rows))
		for i, r := range rows {
			heads[i] = AnchorHead{OrgID: r.OrgID.String(), ChainSeq: r.Seq, Hash: r.Hash}
		}
		var inconsistent []AnchorInconsistency
		if lastID != "" {
			if inconsistent, err = checkConsistency(ctx, q, lastID, heads); err != nil {
				return fmt.Errorf("audit anchor: consistency check: %w", err)
			}
		}
		root := hex.EncodeToString(HeadsRoot(installID.String(), heads))
		aq := auditstore.New(tx)
		b := &batch{}
		b.Install(InstallEvent{
			Actor: System(AnchorActorID), Action: ActionAnchor, TargetKind: "install", TargetID: installID.String(),
			After: anchorAfter{OrgCount: len(heads), HeadsRoot: root},
		})
		w, err := b.write(ctx, aq)
		if err != nil {
			return err
		}
		ev := w.Install[0]
		for _, r := range rows {
			if err := aq.InsertInstallAnchorHead(ctx, auditstore.InsertInstallAnchorHeadParams{
				AnchorEventID: ev.ID, OrgID: r.OrgID, ChainSeq: r.Seq, Hash: r.Hash,
			}); err != nil {
				if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
					return fmt.Errorf("audit anchor: record the heads: %w: %w", ErrInstallWriteDenied, err)
				}
				return fmt.Errorf("audit anchor: record the heads: %w", err)
			}
		}
		res = AnchorResult{
			Written: true, EventID: ev.ID.String(), ChainSeq: ev.ChainSeq, OrgCount: len(heads), HeadsRoot: root,
			InstallHead: hex.EncodeToString(ev.Hash), Inconsistent: inconsistent,
		}
		return nil
	})
	if err != nil {
		return AnchorResult{}, err
	}
	for _, i := range res.Inconsistent {
		anchorInconsistent.WithLabelValues(i.Reason).Inc()
	}
	return res, nil
}

// anchorAge is koinon_audit_anchor_age_seconds: the seconds since the newest anchor (since the process
// started while there is none). It has no labels (ADR 0023).
var anchorAge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "koinon_audit_anchor_age_seconds",
	Help: "Seconds since the newest audit.anchor event in the install chain (since the process started while there is none); an alert on it says anchoring stopped.",
})

// anchorInconsistent is koinon_audit_anchor_inconsistent_total: orgs found inconsistent with the newest
// earlier anchor when a new one was written (Anchor), by reason. The labels are fixed (ADR 0023): never
// an org, agent, user or run ID; the log line names the org.
var anchorInconsistent = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "koinon_audit_anchor_inconsistent_total",
	Help: "Orgs whose audit chain disagreed with the newest earlier anchor when a new anchor was written, by reason (hash_changed, seq_regressed); an alert on any increase says an org chain was rewritten or rolled back.",
}, []string{"reason"})

// RegisterAnchorMetric registers koinon_audit_anchor_age_seconds and koinon_audit_anchor_inconsistent_total
// (AnchorJob, Anchor) on reg. Only koinon-api anchors, so only koinon-api registers them.
func RegisterAnchorMetric(reg prometheus.Registerer) {
	for _, r := range []string{InconsistentHashChanged, InconsistentSeqRegressed} {
		anchorInconsistent.WithLabelValues(r)
	}
	reg.MustRegister(anchorAge, anchorInconsistent)
}

// AnchorDB is what AnchorJob needs of the database: transactions and plain queries (*pgxpool.Pool).
type AnchorDB interface {
	Beginner
	store.DBTX
}

// AnchorLogMessage is the message of the log line of a written anchor: operators filter on it to ship the
// witness off the cluster.
const AnchorLogMessage = "anchored the org audit chains"

// AnchorJob anchors the org chains every Interval (ADR 0100 decision 4.1) and keeps
// koinon_audit_anchor_age_seconds. Every replica runs it; Anchor's lock and MinAge make the first one
// that finds an anchor due write it and the others skip, so one anchor is written per interval.
type AnchorJob struct {
	DB       AnchorDB
	Interval time.Duration
	// Poll is how often the job looks at the newest anchor; zero is the smaller of Interval and 30 seconds.
	Poll time.Duration
	Log  *slog.Logger

	started time.Time
	state   string
}

// Tick looks at the newest anchor, sets the gauge and writes an anchor when one is due.
func (j *AnchorJob) Tick(ctx context.Context) {
	if j.started.IsZero() {
		j.started = time.Now()
	}
	age, none := time.Since(j.started).Seconds(), false
	last, err := store.New(j.DB).GetLastInstallAnchor(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		none = true
	case err != nil:
		j.report(ctx, "read", fmt.Errorf("read the newest anchor: %w", err))
		return
	default:
		age = last.AgeSeconds
	}
	anchorAge.Set(age)
	if !none && age < j.Interval.Seconds() {
		return
	}
	res, err := Anchor(ctx, j.DB, AnchorOptions{MinAge: j.Interval})
	switch {
	case err != nil:
		state := "failed"
		if errors.Is(err, ErrInstallWriteDenied) {
			state = "denied"
		}
		j.report(ctx, state, err)
	case res.Written:
		anchorAge.Set(0)
		j.state = ""
		// The success line is a witness the database cannot rewrite once the logs are shipped off the
		// cluster (guides/audit.md, "Anchors"); heads_root and install_head are audit chain hashes, not secrets.
		j.Log.Info(AnchorLogMessage, "chain_seq", res.ChainSeq, "orgs", res.OrgCount,
			"heads_root", res.HeadsRoot, "install_head", res.InstallHead)
		if len(res.Inconsistent) > 0 {
			items := make([]string, len(res.Inconsistent))
			for i, x := range res.Inconsistent {
				items[i] = fmt.Sprintf("%s %s at chain_seq %d", x.Org, x.Reason, x.AnchoredSeq)
			}
			j.Log.Error("org audit chains are inconsistent with the previous anchor; the anchor was written anyway, run the audit verify",
				"count", len(items), "orgs", items)
		}
	default:
		j.state = ""
	}
}

// report logs a failure once per change of state.
func (j *AnchorJob) report(ctx context.Context, state string, err error) {
	if ctx.Err() != nil || state == j.state {
		return
	}
	j.state = state
	if state == "denied" {
		j.Log.Error("the database refuses koinon-api's audit anchor: the install audit writer role is not granted yet; "+
			"retrying (koinon-api migrate --grants-only, the chart's post-upgrade hook)", "err", err)
		return
	}
	j.Log.Error("cannot anchor the org audit chains; retrying", "err", err)
}

// Run ticks until ctx ends.
func (j *AnchorJob) Run(ctx context.Context) {
	poll := j.Poll
	if poll <= 0 {
		poll = min(j.Interval, 30*time.Second)
	}
	for {
		j.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// AnchorBreak is an anchor the database no longer agrees with.
type AnchorBreak struct {
	// AnchorSeq and AnchorID name the anchor event in the install chain.
	AnchorSeq int64
	AnchorID  string
	// Org is the org whose chain does not hold the anchored head; empty when the anchor itself is wrong
	// (its heads do not match its heads_root or org_count).
	Org string
	// Seq is the org chain_seq the anchor names, or AnchorSeq when Org is empty. The database verifier
	// and the file verifier report the same Seq for the same fault.
	Seq    int64
	Reason string
}

func (b AnchorBreak) String() string {
	if b.Org == "" {
		return fmt.Sprintf("anchor at install chain_seq %d (event %s): %s", b.AnchorSeq, b.AnchorID, b.Reason)
	}
	return fmt.Sprintf("org %s: anchor at install chain_seq %d (event %s) names chain_seq %d: %s", b.Org, b.AnchorSeq, b.AnchorID, b.Seq, b.Reason)
}

// VerifyAnchors checks every anchor of the install chain: that the heads in the side table are the
// ones the event's org_count and heads_root commit to, and that each org chain still has the anchored
// hash at the anchored chain_seq. An org in an anchor whose chain is gone fails. An org is reported
// once, at the oldest anchor it fails; an org created after the last anchor is not in it and is not
// checked. It returns the number of anchors checked. The install chain itself is checked by
// VerifyInstall, which calls this when it holds; anchors only mean something over an intact install chain.
func VerifyAnchors(ctx context.Context, q *store.Queries) (int64, []AnchorBreak, error) {
	installID, err := q.GetInstallID(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("audit verify: read the install ID: %w", err)
	}
	var (
		checked  int64
		breaks   []AnchorBreak
		reported = map[string]bool{}
		after    int64
	)
	for {
		page, err := q.ListInstallAnchors(ctx, store.ListInstallAnchorsParams{AfterSeq: after, UpToSeq: math.MaxInt64, PageSize: verifyPage})
		if err != nil {
			return checked, breaks, fmt.Errorf("audit verify: read the anchors: %w", err)
		}
		for _, a := range page {
			after = a.ChainSeq
			checked++
			bad := func(org string, seq int64, format string, args ...any) {
				if org != "" {
					if reported[org] {
						return
					}
					reported[org] = true
				}
				breaks = append(breaks, AnchorBreak{AnchorSeq: a.ChainSeq, AnchorID: a.IDText, Org: org, Seq: seq, Reason: fmt.Sprintf(format, args...)})
			}
			want, err := parseAnchorSummary(a.SummaryText)
			if err != nil {
				bad("", a.ChainSeq, "%v", err)
				continue
			}
			var id pgtype.UUID
			if err := id.Scan(a.IDText); err != nil {
				return checked, breaks, fmt.Errorf("audit verify: anchor id %q: %w", a.IDText, err)
			}
			rows, err := q.CheckAnchoredHeads(ctx, id)
			if err != nil {
				return checked, breaks, fmt.Errorf("audit verify: read the heads of anchor %s: %w", a.IDText, err)
			}
			heads := make([]AnchorHead, len(rows))
			for i, r := range rows {
				heads[i] = AnchorHead{OrgID: r.OrgText, ChainSeq: r.ChainSeq, Hash: r.Hash}
			}
			switch {
			case len(rows) != want.OrgCount:
				bad("", a.ChainSeq, "the anchor commits to %d org chains and the side table holds %d heads for it", want.OrgCount, len(rows))
			case hex.EncodeToString(HeadsRoot(installID.String(), heads)) != want.HeadsRoot:
				bad("", a.ChainSeq, "the anchored heads do not match the heads_root the anchor event commits to: a head was changed")
			}
			for _, r := range rows {
				switch {
				case r.OrgHash == nil:
					bad(r.OrgText, r.ChainSeq, "the org's chain has no row at chain_seq %d: rows were removed behind the anchor, or the chain is gone", r.ChainSeq)
				case !bytes.Equal(r.OrgHash, r.Hash):
					bad(r.OrgText, r.ChainSeq, "the org's chain has another hash at chain_seq %d than the anchored one: it was rewritten behind the anchor", r.ChainSeq)
				}
			}
		}
		if len(page) < verifyPage {
			return checked, breaks, nil
		}
	}
}

// VerifyOrgAnchors checks one org's chain against the install anchors: that the org chain still has
// the anchored hash at the chain_seq of every anchor that names the org (CheckOrgAnchoredHeads, an
// org-filtered read, not every anchor's heads). The org is reported once, at the oldest anchor it fails,
// as VerifyAnchors does. It does not walk the install chain, and anchor faults that name no org (a wrong
// heads_root) are left to VerifyInstall. It returns the number of anchors that name the org and its breaks.
func VerifyOrgAnchors(ctx context.Context, q *store.Queries, org string) (int64, []AnchorBreak, error) {
	var id pgtype.UUID
	if err := id.Scan(org); err != nil {
		return 0, nil, fmt.Errorf("audit verify: org id %q: %w", org, err)
	}
	rows, err := q.CheckOrgAnchoredHeads(ctx, id)
	if err != nil {
		return 0, nil, fmt.Errorf("audit verify: read the anchored heads of org %s: %w", org, err)
	}
	for _, r := range rows {
		var reason string
		switch {
		case r.OrgHash == nil:
			reason = fmt.Sprintf("the org's chain has no row at chain_seq %d: rows were removed behind the anchor, or the chain is gone", r.ChainSeq)
		case !bytes.Equal(r.OrgHash, r.Hash):
			reason = fmt.Sprintf("the org's chain has another hash at chain_seq %d than the anchored one: it was rewritten behind the anchor", r.ChainSeq)
		default:
			continue
		}
		return int64(len(rows)), []AnchorBreak{{AnchorSeq: r.AnchorSeq, AnchorID: r.AnchorText, Org: org, Seq: r.ChainSeq, Reason: reason}}, nil
	}
	return int64(len(rows)), nil, nil
}
