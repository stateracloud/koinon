// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Export writes a chain as JSON lines that anyone can verify without the database (ADR 0100
// decision 5, the audit export page of the docs site, https://koinon.stateracloud.com/docs/operate/audit-export/). VerifyExport checks such a file
// with the hash code the in-database verifier uses (hashRow), so the two cannot disagree.

// ExportFormat is the value of the header's "format" field.
const ExportFormat = "koinon.audit.export.v2"

// ExportAnchorLineKind is the "line" value of a stream line that carries one anchor's org heads
// (immediately after its audit.anchor event).
const ExportAnchorLineKind = "anchor_heads"

// exportPage is how many rows Export reads per query: an export holds one page in memory, never a
// chain.
var exportPage int32 = 1000

// ExportHead is a chain head: the sequence and hash of the last event.
type ExportHead struct {
	ChainSeq int64  `json:"chain_seq"`
	Hash     string `json:"hash"`
}

// ExportHeader is the first line of an export.
type ExportHeader struct {
	Format    string `json:"format"`
	Chain     string `json:"chain"`
	InstallID string `json:"install_id"`
	// OrgID is set for an org chain only.
	OrgID string `json:"org_id,omitempty"`
	// FromSeq is the chain_seq of the first event line; 1 for a whole chain. A partial export is
	// checked against an earlier export or a checkpoint (VerifyExport).
	FromSeq int64 `json:"from_seq"`
	// Head is the chain head when the export started. Events appended later are not in the file.
	Head ExportHead `json:"head"`
	// Anchors is refused in v2: each audit.anchor event is followed by an anchor_heads stream line
	// instead. Kept so a file that still puts them in the header is rejected, not silently ignored.
	Anchors []ExportAnchor `json:"anchors,omitempty"`
}

// ExportAnchorHead is an org chain head an anchor commits to.
type ExportAnchorHead struct {
	OrgID    string `json:"org_id"`
	ChainSeq int64  `json:"chain_seq"`
	Hash     string `json:"hash"`
}

// ExportAnchor is one audit.anchor event with its heads (a verified anchor_heads stream line, or the
// value CollectAnchors gathers for --anchors).
type ExportAnchor struct {
	// ChainSeq and EventID name the anchor event in the install chain.
	ChainSeq int64  `json:"chain_seq"`
	EventID  string `json:"event_id"`
	// OrgCount and HeadsRoot are what the event's summary commits to; Heads must hash to HeadsRoot
	// (HeadsRoot, hex).
	OrgCount  int                `json:"org_count"`
	HeadsRoot string             `json:"heads_root"`
	Heads     []ExportAnchorHead `json:"heads"`
}

// ExportAnchorLine is the JSON line that follows an audit.anchor event in an install export: the org
// heads that event commits to. It is not hashed into the chain; VerifyExport checks it against the
// event's summary (org_count, heads_root) as it streams.
type ExportAnchorLine struct {
	Line      string             `json:"line"` // ExportAnchorLineKind
	ChainSeq  int64              `json:"chain_seq"`
	EventID   string             `json:"event_id"`
	OrgCount  int                `json:"org_count"`
	HeadsRoot string             `json:"heads_root"`
	Heads     []ExportAnchorHead `json:"heads"`
}

// ExportAnchorSet is the anchors of a verified install export, to check an org export against.
// Build it with [ExportResult.AnchorSet], so OrgID is the filter the anchors were collected with.
type ExportAnchorSet struct {
	InstallID string
	// OrgID is the org whose heads alone the anchors keep (ExportVerifyOptions.AnchorOrgID), empty
	// when they keep every org's. VerifyExport refuses an org export of another org: its heads were
	// dropped, so checking it would pass with no anchor checked.
	OrgID   string
	Anchors []ExportAnchor
}

// ExportEvent is one event line. Every field the hash covers is here exactly as hashed.
type ExportEvent struct {
	ChainSeq   int64  `json:"chain_seq"`
	ID         string `json:"id"`
	ActorKind  string `json:"actor_kind"`
	ActorID    string `json:"actor_id"`
	Action     string `json:"action"`
	TargetKind string `json:"target_kind"`
	TargetID   string `json:"target_id"`
	// SummaryText is the text Postgres prints for the jsonb value, which the hash covers; Summary is
	// the same value parsed, for readers. The verifier refuses a file where they differ.
	SummaryText     string          `json:"summary_text"`
	Summary         json.RawMessage `json:"summary"`
	RequestID       string          `json:"request_id"`
	CreatedAtMicros int64           `json:"created_at_micros"`
	PrevHash        string          `json:"prev_hash"`
	Hash            string          `json:"hash"`
}

// ExportScope names the chain to export: the install chain, or one org's.
type ExportScope struct {
	Install bool
	Org     pgtype.UUID
}

// Export writes the chain of sc to w, from chain_seq fromSeq (0 or 1: the whole chain) up to the head
// as it was when the export started. It streams: it pages through the chain by chain_seq, exportPage
// rows per query, and writes each line before reading the next page.
//
// An install export writes each audit.anchor event's org heads as an anchor_heads stream line right
// after that event (never in the header), so header size stays fixed as the anchor count grows.
func Export(ctx context.Context, q *store.Queries, w io.Writer, sc ExportScope, fromSeq int64) error {
	if fromSeq < 0 {
		return fmt.Errorf("audit export: from-seq %d is negative", fromSeq)
	}
	if fromSeq == 0 {
		fromSeq = 1
	}
	if !sc.Install && !sc.Org.Valid {
		return errors.New("audit export: an org chain needs an org")
	}
	if sc.Install && sc.Org.Valid {
		return errors.New("audit export: the install chain has no org")
	}
	installID, err := q.GetInstallID(ctx)
	if err != nil {
		return fmt.Errorf("audit export: read the install ID: %w", err)
	}
	hdr := ExportHeader{Format: ExportFormat, Chain: ChainInstall, InstallID: installID.String(), FromSeq: fromSeq}
	if sc.Install {
		head, err := q.GetInstallAuditChainHead(ctx)
		if err != nil {
			return fmt.Errorf("audit export: read the install chain head: %w", err)
		}
		hdr.Head = ExportHead{ChainSeq: head.Seq, Hash: hex.EncodeToString(head.Hash)}
	} else {
		hdr.Chain, hdr.OrgID = ChainOrg, sc.Org.String()
		head, err := q.GetAuditChainHead(ctx, sc.Org)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// An org's head appears with its first event: no head is an empty chain.
			hdr.Head = ExportHead{Hash: hex.EncodeToString(ZeroHash)}
		case err != nil:
			return fmt.Errorf("audit export: read the head of org %s: %w", sc.Org.String(), err)
		default:
			hdr.Head = ExportHead{ChainSeq: head.Seq, Hash: hex.EncodeToString(head.Hash)}
		}
	}
	if fromSeq > hdr.Head.ChainSeq+1 {
		// Nothing has been written yet: a caller that streams the output can still answer with an error.
		return fmt.Errorf("%w: from-seq %d is beyond the chain, whose head is chain_seq %d", ErrBadFilter, fromSeq, hdr.Head.ChainSeq)
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(hdr); err != nil {
		return fmt.Errorf("audit export: write: %w", err)
	}
	after := fromSeq - 1
	for after < hdr.Head.ChainSeq {
		page, err := exportRows(ctx, q, sc, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return fmt.Errorf("audit export: the chain ends at chain_seq %d but its head says %d: rows are missing", after, hdr.Head.ChainSeq)
		}
		headsByAnchor, err := exportAnchorHeadsForPage(ctx, q, sc, page, hdr.Head.ChainSeq)
		if err != nil {
			return err
		}
		for _, ev := range page {
			if ev.ChainSeq > hdr.Head.ChainSeq {
				if after < hdr.Head.ChainSeq {
					// Rows at or below the head are gone and a newer event took their place: without this
					// the loop would query the same page forever.
					return fmt.Errorf("audit export: the chain ends at chain_seq %d but its head says %d: rows are missing", after, hdr.Head.ChainSeq)
				}
				break // appended after the export started
			}
			if ev.ChainSeq != after+1 {
				return fmt.Errorf("audit export: chain_seq %d follows %d: rows are missing", ev.ChainSeq, after)
			}
			if err := enc.Encode(ev); err != nil {
				return fmt.Errorf("audit export: write: %w", err)
			}
			if sc.Install && ev.Action == ActionAnchor {
				if err := enc.Encode(exportAnchorLine(ev, headsByAnchor[ev.ID])); err != nil {
					return fmt.Errorf("audit export: write: %w", err)
				}
			}
			after = ev.ChainSeq
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("audit export: write: %w", err)
	}
	return nil
}

// exportAnchorHeadsForPage loads the org heads for every audit.anchor event in page whose chain_seq
// is at or below headSeq. Memory is bounded by the page: one query for the page's anchor range.
func exportAnchorHeadsForPage(ctx context.Context, q *store.Queries, sc ExportScope, page []ExportEvent, headSeq int64) (map[string][]ExportAnchorHead, error) {
	if !sc.Install {
		return nil, nil
	}
	var lo, hi int64
	var any bool
	for _, ev := range page {
		if ev.Action != ActionAnchor || ev.ChainSeq > headSeq {
			continue
		}
		if !any || ev.ChainSeq < lo {
			lo = ev.ChainSeq
		}
		if !any || ev.ChainSeq > hi {
			hi = ev.ChainSeq
		}
		any = true
	}
	if !any {
		return nil, nil
	}
	heads, err := q.ListInstallAnchorHeadsInRange(ctx, store.ListInstallAnchorHeadsInRangeParams{FromSeq: lo, UpToSeq: hi})
	if err != nil {
		return nil, fmt.Errorf("audit export: read the anchored heads: %w", err)
	}
	out := map[string][]ExportAnchorHead{}
	for _, h := range heads {
		out[h.AnchorText] = append(out[h.AnchorText], ExportAnchorHead{OrgID: h.OrgText, ChainSeq: h.ChainSeq, Hash: hex.EncodeToString(h.Hash)})
	}
	return out, nil
}

// exportAnchorLine builds the stream line that follows an audit.anchor event.
func exportAnchorLine(ev ExportEvent, heads []ExportAnchorHead) ExportAnchorLine {
	line := ExportAnchorLine{Line: ExportAnchorLineKind, ChainSeq: ev.ChainSeq, EventID: ev.ID, Heads: heads}
	if line.Heads == nil {
		line.Heads = []ExportAnchorHead{}
	}
	// A summary that cannot be read is exported as it is: the verifier reports it at this chain_seq.
	if sum, err := parseAnchorSummary(ev.SummaryText); err == nil {
		line.OrgCount, line.HeadsRoot = sum.OrgCount, sum.HeadsRoot
	}
	return line
}

// exportRows reads one page of events after afterSeq as export lines.
func exportRows(ctx context.Context, q *store.Queries, sc ExportScope, afterSeq int64) ([]ExportEvent, error) {
	var out []ExportEvent
	if sc.Install {
		rows, err := q.ListInstallAuditChainRows(ctx, store.ListInstallAuditChainRowsParams{AfterSeq: afterSeq, PageSize: exportPage})
		if err != nil {
			return nil, fmt.Errorf("audit export: read the install chain: %w", err)
		}
		for _, r := range rows {
			out = append(out, exportEvent(r.ChainSeq, r.IDText, r.ActorKind, r.ActorID, r.Action, r.TargetKind, r.TargetID,
				r.SummaryText, r.RequestID, r.CreatedMicros, r.PrevHash, r.Hash))
		}
		return out, nil
	}
	rows, err := q.ListAuditChainRows(ctx, store.ListAuditChainRowsParams{OrgID: sc.Org, AfterSeq: afterSeq, PageSize: exportPage})
	if err != nil {
		return nil, fmt.Errorf("audit export: read org %s: %w", sc.Org.String(), err)
	}
	for _, r := range rows {
		out = append(out, exportEvent(r.ChainSeq, r.IDText, r.ActorKind, r.ActorID, r.Action, r.TargetKind, r.TargetID,
			r.SummaryText, r.RequestID, r.CreatedMicros, r.PrevHash, r.Hash))
	}
	return out, nil
}

func exportEvent(seq int64, id, kind, actor, action, tkind, target, summary, req string, micros int64, prev, hash []byte) ExportEvent {
	return ExportEvent{
		ChainSeq: seq, ID: id, ActorKind: kind, ActorID: actor, Action: action, TargetKind: tkind, TargetID: target,
		SummaryText: summary, Summary: json.RawMessage(summary), RequestID: req, CreatedAtMicros: micros,
		PrevHash: hex.EncodeToString(prev), Hash: hex.EncodeToString(hash),
	}
}

// ExportEnd is where a verified export ends: the hash a later partial export must start from.
type ExportEnd struct {
	Chain, InstallID, OrgID string
	Seq                     int64
	Hash                    []byte
}

// ExportVerifyOptions tunes VerifyExport.
type ExportVerifyOptions struct {
	// After is the end of an earlier, verified export. A partial export (from_seq above 1) starts
	// from its hash: its first prev_hash must equal it.
	After *ExportEnd
	// Anchors are the anchors of an install export that verified (VerifyExport of it with
	// CollectAnchors, then its StreamAnchors). An org export is checked against them: at each
	// anchored chain_seq of its org the file must hold the anchored hash, so a chain rewritten
	// behind an anchor fails at the same chain_seq the database verifier names. Only for an org
	// export of the same install.
	Anchors *ExportAnchorSet
	// CollectAnchors, on an install export, keeps each verified anchor_heads line on
	// ExportResult.StreamAnchors (for --anchors). Off by default so verify memory stays bounded.
	CollectAnchors bool
	// AnchorOrgID, with CollectAnchors, keeps on each collected anchor only the head of this org
	// (the org export the anchors are for): checkOrgAnchors reads no other, so memory stays
	// bounded by the anchor count, not anchors times orgs. Empty keeps every head. The heads are
	// still all checked against heads_root before they are dropped.
	AnchorOrgID string
}

// ExportResult is the verdict on an export file.
type ExportResult struct {
	Header ExportHeader
	// Rows is the number of event lines checked up to the break.
	Rows int64
	// Break is nil when the file holds.
	Break *Break
	// Partial is set when the file starts after chain_seq 1; PrevChecked then says whether the first
	// prev_hash was compared with an earlier export.
	Partial, PrevChecked bool
	// End is the last event line checked (its hash is what a later export continues from).
	End ExportEnd
	// Anchors is the number of anchors checked: those of an install export against their stream
	// lines, or those of ExportVerifyOptions.Anchors that name this org chain, against the file.
	// AnchorsUnchecked counts the anchored heads of this org below the file's first chain_seq, which
	// a partial file cannot show.
	Anchors, AnchorsUnchecked int
	// StreamAnchors are the verified anchor_heads lines of an install export when CollectAnchors
	// was set. Used to pin an org export (--anchors). Empty otherwise so memory stays bounded.
	StreamAnchors []ExportAnchor
	// AnchorOrgID is the ExportVerifyOptions.AnchorOrgID StreamAnchors were collected with.
	AnchorOrgID string
}

// OK reports whether the file holds.
func (r ExportResult) OK() bool { return r.Break == nil }

// AnchorSet is the anchors r collected (CollectAnchors), to check an org export against with
// ExportVerifyOptions.Anchors. It carries the org the heads were kept for.
func (r ExportResult) AnchorSet() *ExportAnchorSet {
	return &ExportAnchorSet{InstallID: r.Header.InstallID, OrgID: r.AnchorOrgID, Anchors: r.StreamAnchors}
}

// ErrExportFormat is returned for a file that is not an export the verifier can read.
var ErrExportFormat = errors.New("audit export: not a koinon.audit.export.v2 file")

// VerifyExport checks an export read from r, with no database. It streams: one event at a time. It
// checks that chain_seq counts up without gaps from the header's from_seq, that each prev_hash is the
// previous line's hash, that each hash matches the line's content (the same hashRow as the in-database
// verifier), that summary equals summary_text, and that the last line is the head the header names, so
// a file with lines removed from the end fails. After each audit.anchor event it reads the following
// anchor_heads line and checks it before reading on, so memory does not grow with the anchor count.
// Its first break is the first chain_seq that does not fit, exactly as audit verify reports for the
// database. An error (not a break) means the file or the options cannot be checked at all.
func VerifyExport(r io.Reader, opt ExportVerifyOptions) (ExportResult, error) {
	var res ExportResult
	dec := json.NewDecoder(bufio.NewReader(r))
	if err := dec.Decode(&res.Header); err != nil {
		return res, fmt.Errorf("%w: the first line is not a header: %w", ErrExportFormat, err)
	}
	h := res.Header
	if h.Format != ExportFormat {
		return res, fmt.Errorf("%w: format %q", ErrExportFormat, h.Format)
	}
	if len(h.Anchors) > 0 {
		return res, fmt.Errorf("%w: anchors belong in stream lines, not the header", ErrExportFormat)
	}
	tag, scope := hashTag, h.OrgID
	switch h.Chain {
	case ChainInstall:
		tag, scope = installHashTag, h.InstallID
		if h.OrgID != "" {
			return res, fmt.Errorf("%w: an install export has an org_id", ErrExportFormat)
		}
		if opt.Anchors != nil {
			return res, errors.New("audit export: --anchors is for an org export; this one is the install chain")
		}
		if opt.CollectAnchors {
			res.AnchorOrgID = opt.AnchorOrgID
		}
	case ChainOrg:
		if h.OrgID == "" {
			return res, fmt.Errorf("%w: an org export has no org_id", ErrExportFormat)
		}
		if a := opt.Anchors; a != nil && a.InstallID != h.InstallID {
			return res, errors.New("audit export: the anchors are of another install")
		}
		if a := opt.Anchors; a != nil && a.OrgID != "" && a.OrgID != h.OrgID {
			return res, fmt.Errorf("audit export: the anchors keep the heads of org %s only; this export is of org %s", a.OrgID, h.OrgID)
		}
		if opt.CollectAnchors {
			return res, errors.New("audit export: CollectAnchors is for an install export")
		}
	default:
		return res, fmt.Errorf("%w: chain %q", ErrExportFormat, h.Chain)
	}
	if scope == "" {
		return res, fmt.Errorf("%w: no ID to hash the chain with", ErrExportFormat)
	}
	headHash, err := decodeHash(h.Head.Hash)
	if err != nil {
		return res, fmt.Errorf("%w: head hash: %w", ErrExportFormat, err)
	}
	if h.Head.ChainSeq < 0 {
		return res, fmt.Errorf("%w: head chain_seq %d", ErrExportFormat, h.Head.ChainSeq)
	}
	if h.FromSeq < 1 {
		return res, fmt.Errorf("%w: from_seq %d", ErrExportFormat, h.FromSeq)
	}

	start, startPrev := h.FromSeq-1, []byte(nil)
	res.Partial = h.FromSeq > 1
	switch {
	case !res.Partial:
		if opt.After != nil {
			return res, errors.New("audit export: --after is for a partial export; this one starts at chain_seq 1")
		}
		startPrev = ZeroHash
	case opt.After != nil:
		a := opt.After
		if a.Chain != h.Chain || a.InstallID != h.InstallID || a.OrgID != h.OrgID {
			return res, errors.New("audit export: the earlier export is of another chain or install")
		}
		if a.Seq != start {
			return res, fmt.Errorf("audit export: the earlier export ends at chain_seq %d, this one starts at %d", a.Seq, h.FromSeq)
		}
		startPrev, res.PrevChecked = a.Hash, true
	}

	next := h.FromSeq
	var failed bool
	wanted := map[int64]bool{} // org export: the anchored chain_seqs of this org
	if opt.Anchors != nil {
		for _, a := range opt.Anchors.Anchors {
			for _, ah := range a.Heads {
				if ah.OrgID == h.OrgID {
					wanted[ah.ChainSeq] = true
				}
			}
		}
	}
	orgHashes := map[int64][]byte{} // org export: the hash of each wanted chain_seq in the file
	rows, last, prev, brk, err := walkFrom(start, startPrev, func(int64) ([]link, error) {
		var page []link
		for len(page) < verifyPage && !failed {
			var raw json.RawMessage
			err := dec.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				failed = true
				page = append(page, link{seq: next, bad: fmt.Sprintf("the line after chain_seq %d is not an event: %v", next-1, err)})
				break
			}
			var kind struct {
				Line string `json:"line"`
			}
			_ = json.Unmarshal(raw, &kind)
			if kind.Line == ExportAnchorLineKind {
				failed = true
				page = append(page, link{seq: next, bad: "an anchor_heads line has no preceding audit.anchor event"})
				break
			}
			var ev ExportEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				failed = true
				page = append(page, link{seq: next, bad: fmt.Sprintf("the line after chain_seq %d is not an event: %v", next-1, err)})
				break
			}
			l := exportLink(tag, scope, ev)
			if l.bad == "" && tag == installHashTag && ev.Action == ActionAnchor {
				if msg := readAndCheckAnchorLine(dec, h.InstallID, ev, opt, &res); msg != "" {
					l.bad = msg
				}
			}
			if l.bad == "" && wanted[ev.ChainSeq] {
				orgHashes[ev.ChainSeq] = l.hash
			}
			// encoding/json keeps the last of two keys and another reader may keep the first, so a
			// line with a key twice says two things: refuse it, as a summary with a key twice is (a key
			// twice inside the summary is already reported as such). With chain_seq itself twice its
			// value cannot be trusted: the line is broken at the chain_seq that was due there.
			// Another fault found by exportLink does not keep the forged chain_seq: it is replaced
			// whether or not the line is already bad.
			if k, dup := duplicateKey(raw); dup {
				if k == "chain_seq" {
					l.seq = next
				}
				if l.bad == "" {
					l.bad = fmt.Sprintf("the line has the key %q twice", k)
				}
			}
			page = append(page, l)
			next = l.seq + 1
		}
		return page, nil
	})
	res.Rows, res.Break = rows, brk
	res.End = ExportEnd{Chain: h.Chain, InstallID: h.InstallID, OrgID: h.OrgID, Seq: last, Hash: prev}
	if err != nil || brk != nil {
		return res, err
	}
	if prev == nil { // an empty partial file with no earlier export: nothing to compare the hash with
		prev = headHash
	}
	res.Break = headBreak(true, true, h.Head.ChainSeq, headHash, last, prev)
	if res.Break == nil && h.Chain == ChainOrg {
		res.Break = checkOrgAnchors(&res, opt, orgHashes)
	}
	return res, nil
}

// readAndCheckAnchorLine reads the anchor_heads line after an audit.anchor event and checks it
// against the event. On success it counts the anchor on res and, when opt.CollectAnchors is set,
// appends it to StreamAnchors (with the heads of opt.AnchorOrgID only, when that is set). The line
// is parsed like an event line: a key twice in it is refused. The returned string is a break
// reason, or empty when the line holds.
func readAndCheckAnchorLine(dec *json.Decoder, installID string, ev ExportEvent, opt ExportVerifyOptions, res *ExportResult) string {
	var raw json.RawMessage
	err := dec.Decode(&raw)
	if errors.Is(err, io.EOF) {
		return fmt.Sprintf("the anchor event %s has no following anchor_heads line: its anchored heads were removed from the file", ev.ID)
	}
	var al ExportAnchorLine
	if err == nil {
		err = json.Unmarshal(raw, &al)
	}
	switch {
	case err != nil:
		return fmt.Sprintf("the line after the anchor event %s is not an anchor_heads line: %v", ev.ID, err)
	case al.Line != ExportAnchorLineKind:
		return fmt.Sprintf("the line after the anchor event %s is not an anchor_heads line", ev.ID)
	}
	if k, dup := duplicateKey(raw); dup {
		return fmt.Sprintf("the anchor_heads line after the anchor event %s has the key %q twice", ev.ID, k)
	}
	want, err := parseAnchorSummary(ev.SummaryText)
	if err != nil {
		return err.Error()
	}
	if al.ChainSeq != ev.ChainSeq {
		return fmt.Sprintf("the anchor_heads line names chain_seq %d, the event is %d", al.ChainSeq, ev.ChainSeq)
	}
	if al.EventID != ev.ID {
		return fmt.Sprintf("the anchor_heads line names event %s, the chain has %s", al.EventID, ev.ID)
	}
	if al.OrgCount != want.OrgCount || al.HeadsRoot != want.HeadsRoot {
		return "the anchor_heads line's org_count and heads_root are not the ones the anchor event commits to"
	}
	if len(al.Heads) != want.OrgCount {
		return fmt.Sprintf("the anchor commits to %d org chains and the stream holds %d heads for it", want.OrgCount, len(al.Heads))
	}
	heads, err := anchorHeads(al.Heads)
	if err != nil {
		return err.Error()
	}
	if hex.EncodeToString(HeadsRoot(installID, heads)) != want.HeadsRoot {
		return "the anchored heads do not match the heads_root the anchor event commits to: a head was forged or removed"
	}
	res.Anchors++
	if opt.CollectAnchors {
		kept := al.Heads
		if opt.AnchorOrgID != "" {
			kept = nil // a fresh slice: al.Heads (every org) is released with the line
			for _, h := range al.Heads {
				if h.OrgID == opt.AnchorOrgID {
					kept = append(kept, h)
				}
			}
		}
		res.StreamAnchors = append(res.StreamAnchors, ExportAnchor{
			ChainSeq: al.ChainSeq, EventID: al.EventID, OrgCount: al.OrgCount, HeadsRoot: al.HeadsRoot, Heads: kept,
		})
	}
	return ""
}

// anchorHeads decodes the heads of an anchor_heads stream line; it refuses a malformed or repeated head.
func anchorHeads(in []ExportAnchorHead) ([]AnchorHead, error) {
	out := make([]AnchorHead, len(in))
	seen := map[string]bool{}
	for i, h := range in {
		hash, err := decodeHash(h.Hash)
		if err != nil {
			return nil, fmt.Errorf("anchored head of org %s: hash: %w", h.OrgID, err)
		}
		if h.ChainSeq < 1 || h.OrgID == "" || seen[h.OrgID] {
			return nil, fmt.Errorf("anchored head of org %q at chain_seq %d is malformed or repeated", h.OrgID, h.ChainSeq)
		}
		seen[h.OrgID] = true
		out[i] = AnchorHead{OrgID: h.OrgID, ChainSeq: h.ChainSeq, Hash: hash}
	}
	return out, nil
}

// checkOrgAnchors checks an org export against the anchors of a verified install export.
func checkOrgAnchors(res *ExportResult, opt ExportVerifyOptions, orgHashes map[int64][]byte) *Break {
	if opt.Anchors == nil {
		return nil
	}
	h := res.Header
	for _, a := range opt.Anchors.Anchors { // in anchor order: the oldest failing anchor wins
		for _, ah := range a.Heads {
			if ah.OrgID != h.OrgID {
				continue
			}
			switch {
			case ah.ChainSeq < h.FromSeq:
				res.AnchorsUnchecked++
			case ah.ChainSeq > res.End.Seq:
				return &Break{Seq: ah.ChainSeq, Reason: fmt.Sprintf("anchor at install chain_seq %d: the file ends at chain_seq %d, before the anchored chain_seq %d: rows were removed behind the anchor, or the file is older than the anchor (export the org after the install export)", a.ChainSeq, res.End.Seq, ah.ChainSeq)}
			default:
				res.Anchors++
				want, err := decodeHash(ah.Hash)
				if err != nil || !bytes.Equal(orgHashes[ah.ChainSeq], want) {
					return &Break{Seq: ah.ChainSeq, Reason: fmt.Sprintf("anchor at install chain_seq %d names another hash at chain_seq %d than the file has: the chain was rewritten behind the anchor", a.ChainSeq, ah.ChainSeq)}
				}
			}
		}
	}
	return nil
}

// exportLink turns a line into a walker link: the stored values and the hash recomputed from the
// line's content. A line the verifier cannot read as an event carries bad.
func exportLink(tag, scope string, ev ExportEvent) link {
	l := link{seq: ev.ChainSeq, id: ev.ID}
	prev, perr := decodeHash(ev.PrevHash)
	hash, herr := decodeHash(ev.Hash)
	switch {
	case perr != nil:
		l.bad = "prev_hash: " + perr.Error()
		return l
	case herr != nil:
		l.bad = "hash: " + herr.Error()
		return l
	}
	if k, dup := duplicateKey(ev.Summary); dup {
		l.bad = fmt.Sprintf("summary has the key %q twice", k)
		return l
	}
	if !summaryMatches(ev.SummaryText, ev.Summary) {
		l.bad = "summary does not match summary_text"
		return l
	}
	l.prev, l.hash = prev, hash
	l.want = hashRow(tag, chainFields{
		Scope: scope, ID: ev.ID, Seq: ev.ChainSeq, ActorKind: ev.ActorKind, ActorID: ev.ActorID, Action: ev.Action,
		TargetKind: ev.TargetKind, TargetID: ev.TargetID, Summary: ev.SummaryText, RequestID: ev.RequestID,
		CreatedMicros: ev.CreatedAtMicros, PrevHash: prev,
	})
	return l
}

func decodeHash(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(ZeroHash) {
		return nil, errors.New("not 64 hex digits")
	}
	return b, nil
}

// summaryMatches reports whether the parsed summary is the value summary_text spells.
func summaryMatches(text string, parsed json.RawMessage) bool {
	var a, b any
	for _, p := range []struct {
		src []byte
		dst *any
	}{{[]byte(text), &a}, {parsed, &b}} {
		dec := json.NewDecoder(bytes.NewReader(p.src))
		dec.UseNumber()
		if dec.Decode(p.dst) != nil {
			return false
		}
	}
	return reflect.DeepEqual(a, b)
}

// duplicateKey returns a key that an object in raw, at any depth, holds twice, and whether there is
// one. encoding/json keeps the last of two equal keys and other readers may keep the first, so a
// summary with one could show a reader a value the hashed summary_text does not hold; it is refused.
// Keys are compared unescaped ("a" and "\u0061" are the same key). A syntax error is not reported
// here: summaryMatches refuses the summary for it.
func duplicateKey(raw []byte) (string, bool) {
	type object struct {
		keys    map[string]struct{}
		wantKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []*object // nil entries are arrays
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		var top *object
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if t, ok := tok.(string); ok && top != nil && top.wantKey {
			if _, dup := top.keys[t]; dup {
				return t, true
			}
			top.keys[t], top.wantKey = struct{}{}, false
			continue
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			continue
		}
		// A value: the object holding it wants its next key.
		if top != nil {
			top.wantKey = true
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &object{keys: map[string]struct{}{}, wantKey: true})
		case json.Delim('['):
			stack = append(stack, nil)
		}
	}
}

// WriteExportReport prints the verdict on an export and reports whether it holds.
func WriteExportReport(w io.Writer, r ExportResult) bool {
	name := "install chain"
	if r.Header.Chain == ChainOrg {
		name = "org " + r.Header.OrgID
	}
	if !r.OK() {
		_, _ = fmt.Fprintf(w, "BROKEN  %s (file): first broken link at chain_seq %d: %s\n", name, r.Break.Seq, r.Break.Reason)
		return false
	}
	_, _ = fmt.Fprintf(w, "ok      %s (file): %d events, chain_seq %d to %d\n", name, r.Rows, r.Header.FromSeq, r.End.Seq)
	if r.Anchors > 0 {
		what := "match their events and heads_root"
		if r.Header.Chain == ChainOrg {
			what = "hold in this chain"
		}
		_, _ = fmt.Fprintf(w, "ok      %d anchors %s\n", r.Anchors, what)
	}
	if r.AnchorsUnchecked > 0 {
		_, _ = fmt.Fprintf(w, "note    %d anchored heads of this org are below the file's first chain_seq and were not checked\n", r.AnchorsUnchecked)
	}
	if r.Partial && !r.PrevChecked {
		_, _ = fmt.Fprintf(w, "note    the file starts at chain_seq %d: its first prev_hash is not checked (give the earlier export with --after)\n", r.Header.FromSeq)
	}
	return true
}
