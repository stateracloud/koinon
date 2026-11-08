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
	// EventID, ChainSeq, OrgCount and HeadsRoot (hex) describe the anchor written.
	EventID   string
	ChainSeq  int64
	OrgCount  int
	HeadsRoot string
}

// Anchor writes one audit.anchor install event and the heads it commits to, in one transaction. The
// domain lock comes first (one anchor at a time across replicas), then the org heads are read as a plain
// read of committed rows, so no anchor names a head that is not committed (an org write in flight holds
// its head row and is not seen), and the install head is taken last by the event insert (ADR 0100
// decision 1.7). A database that refuses the insert for lack of privilege returns ErrInstallWriteDenied.
func Anchor(ctx context.Context, db Beginner, opt AnchorOptions) (AnchorResult, error) {
	var res AnchorResult
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		res = AnchorResult{}
		q := store.New(tx)
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
		if opt.MinAge > 0 {
			last, err := q.GetLastInstallAnchor(ctx)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return fmt.Errorf("audit anchor: read the newest anchor: %w", err)
			case last.AgeSeconds < opt.MinAge.Seconds():
				res.Skipped = "recent"
				return nil
			}
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
		res = AnchorResult{Written: true, EventID: ev.ID.String(), ChainSeq: ev.ChainSeq, OrgCount: len(heads), HeadsRoot: root}
		return nil
	})
	if err != nil {
		return AnchorResult{}, err
	}
	return res, nil
}

// anchorAge is koinon_audit_anchor_age_seconds: the seconds since the newest anchor (since the process
// started while there is none). It has no labels (ADR 0023).
var anchorAge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "koinon_audit_anchor_age_seconds",
	Help: "Seconds since the newest audit.anchor event in the install chain (since the process started while there is none); an alert on it says anchoring stopped.",
})

// RegisterAnchorMetric registers koinon_audit_anchor_age_seconds (AnchorJob) on reg. Only koinon-api
// anchors, so only koinon-api registers it.
func RegisterAnchorMetric(reg prometheus.Registerer) { reg.MustRegister(anchorAge) }

// AnchorDB is what AnchorJob needs of the database: transactions and plain queries (*pgxpool.Pool).
type AnchorDB interface {
	Beginner
	store.DBTX
}

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
		j.Log.Info("anchored the org audit chains", "chain_seq", res.ChainSeq, "orgs", res.OrgCount)
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

// VerifyOrgAnchors checks one org's chain against the install anchors: the breaks of VerifyAnchors
// that name org (its canonical lower-case UUID text). It does not walk the install chain, and anchor
// faults that name no org (a wrong heads_root) are left to VerifyInstall. It returns the number of
// anchors read and the org's breaks.
func VerifyOrgAnchors(ctx context.Context, q *store.Queries, org string) (int64, []AnchorBreak, error) {
	checked, all, err := VerifyAnchors(ctx, q)
	if err != nil {
		return checked, nil, err
	}
	var breaks []AnchorBreak
	for _, b := range all {
		if b.Org == org {
			breaks = append(breaks, b)
		}
	}
	return checked, breaks, nil
}
