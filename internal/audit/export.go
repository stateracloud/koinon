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
const ExportFormat = "koinon.audit.export.v1"

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
	// Anchors are the audit.anchor events of an install export with the org heads each commits to
	// (ADR 0100 decision 4.1): those with a chain_seq in the file. The verifier recomputes each
	// heads_root and refuses a file with an anchor event that has no entry here, or an entry that has no
	// event. An org export is checked against them with ExportVerifyOptions.Anchors.
	Anchors []ExportAnchor `json:"anchors,omitempty"`
}

// ExportAnchorHead is an org chain head an anchor commits to.
type ExportAnchorHead struct {
	OrgID    string `json:"org_id"`
	ChainSeq int64  `json:"chain_seq"`
	Hash     string `json:"hash"`
}

// ExportAnchor is one audit.anchor event with its heads, as the install export header carries it.
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

// ExportAnchorSet is the anchors of a verified install export, to check an org export against.
type ExportAnchorSet struct {
	InstallID string
	Anchors   []ExportAnchor
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
// The install export's header carries the heads of every anchor whose event is in the file (exportAnchors).
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
	if err := exportAnchors(ctx, q, &hdr); err != nil {
		return err
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
			after = ev.ChainSeq
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("audit export: write: %w", err)
	}
	return nil
}

// exportAnchors adds each anchor's heads to the install export's header (ADR 0100 decisions 4.1 and 5):
// the anchors whose event has a chain_seq from the header's from_seq to its head. An org export has
// none. VerifyExport has the matching check (checkAnchors). The header is one line held in memory: an
// install chain anchors hourly, so a long chain is exported in parts with --from-seq.
func exportAnchors(ctx context.Context, q *store.Queries, hdr *ExportHeader) error {
	if hdr.Chain != ChainInstall {
		return nil
	}
	byID := map[string]int{}
	for after := hdr.FromSeq - 1; ; {
		page, err := q.ListInstallAnchors(ctx, store.ListInstallAnchorsParams{AfterSeq: after, UpToSeq: hdr.Head.ChainSeq, PageSize: verifyPage})
		if err != nil {
			return fmt.Errorf("audit export: read the anchors: %w", err)
		}
		for _, a := range page {
			after = a.ChainSeq
			ea := ExportAnchor{ChainSeq: a.ChainSeq, EventID: a.IDText, Heads: []ExportAnchorHead{}}
			// A summary that cannot be read is exported as it is: the verifier reports it at this chain_seq.
			if sum, err := parseAnchorSummary(a.SummaryText); err == nil {
				ea.OrgCount, ea.HeadsRoot = sum.OrgCount, sum.HeadsRoot
			}
			byID[a.IDText] = len(hdr.Anchors)
			hdr.Anchors = append(hdr.Anchors, ea)
		}
		if len(page) < verifyPage {
			break
		}
	}
	if len(hdr.Anchors) == 0 {
		return nil
	}
	heads, err := q.ListInstallAnchorHeadsInRange(ctx, store.ListInstallAnchorHeadsInRangeParams{FromSeq: hdr.FromSeq, UpToSeq: hdr.Head.ChainSeq})
	if err != nil {
		return fmt.Errorf("audit export: read the anchored heads: %w", err)
	}
	for _, h := range heads {
		i, ok := byID[h.AnchorText]
		if !ok {
			continue // appended between the two reads, past the head
		}
		hdr.Anchors[i].Heads = append(hdr.Anchors[i].Heads, ExportAnchorHead{OrgID: h.OrgText, ChainSeq: h.ChainSeq, Hash: hex.EncodeToString(h.Hash)})
	}
	return nil
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
	// Anchors are the anchors of an install export that verified (VerifyExport of it, then its
	// Header.Anchors). An org export is checked against them: at each anchored chain_seq of its org the
	// file must hold the anchored hash, so a chain rewritten behind an anchor fails at the same
	// chain_seq the database verifier names. Only for an org export of the same install.
	Anchors *ExportAnchorSet
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
	// Anchors is the number of anchors checked: those of an install export against its events, or those
	// of ExportVerifyOptions.Anchors that name this org chain, against the file. AnchorsUnchecked counts
	// the anchored heads of this org below the file's first chain_seq, which a partial file cannot show.
	Anchors, AnchorsUnchecked int
}

// OK reports whether the file holds.
func (r ExportResult) OK() bool { return r.Break == nil }

// ErrExportFormat is returned for a file that is not an export the verifier can read.
var ErrExportFormat = errors.New("audit export: not a koinon.audit.export.v1 file")

// VerifyExport checks an export read from r, with no database. It streams: one event at a time. It
// checks that chain_seq counts up without gaps from the header's from_seq, that each prev_hash is the
// previous line's hash, that each hash matches the line's content (the same hashRow as the in-database
// verifier), that summary equals summary_text, and that the last line is the head the header names, so
// a file with lines removed from the end fails. Its first break is the first chain_seq that does not
// fit, exactly as audit verify reports for the database. An error (not a break) means the file or the
// options cannot be checked at all.
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
	case ChainOrg:
		if h.OrgID == "" {
			return res, fmt.Errorf("%w: an org export has no org_id", ErrExportFormat)
		}
		if len(h.Anchors) > 0 {
			return res, fmt.Errorf("%w: an org export has anchors", ErrExportFormat)
		}
		if a := opt.Anchors; a != nil && a.InstallID != h.InstallID {
			return res, errors.New("audit export: the anchors are of another install")
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
	anchorEvents := map[int64]anchorEvent{} // install export: the audit.anchor events of the file
	wanted := map[int64]bool{}              // org export: the anchored chain_seqs of this org
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
			var ev ExportEvent
			err := dec.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err == nil {
				err = json.Unmarshal(raw, &ev)
			}
			if err != nil {
				failed = true
				page = append(page, link{seq: next, bad: fmt.Sprintf("the line after chain_seq %d is not an event: %v", next-1, err)})
				break
			}
			l := exportLink(tag, scope, ev)
			if l.bad == "" {
				if tag == installHashTag && ev.Action == ActionAnchor {
					anchorEvents[ev.ChainSeq] = anchorEvent{id: ev.ID, summary: ev.SummaryText}
				}
				if wanted[ev.ChainSeq] {
					orgHashes[ev.ChainSeq] = l.hash
				}
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
	if res.Break == nil {
		res.Break = checkAnchors(&res, opt, anchorEvents, orgHashes)
	}
	return res, nil
}

// anchorEvent is an audit.anchor event line of an install export.
type anchorEvent struct{ id, summary string }

// checkAnchors checks the anchors of a verified chain (ADR 0100 decision 4.1); issue 7 (checkpoints)
// adds the signed checkpoint opt names here. For an install export, every anchor event of the file has an
// entry in the header, every entry is an anchor event of the file, and each entry's heads hash to the
// heads_root and org_count the event commits to; the break is at the lowest anchor chain_seq that fails.
// For an org export with opt.Anchors, each anchored head of this org that the file covers must hold the
// anchored hash; the break is at the lowest anchored chain_seq that fails, which is the chain_seq the
// database verifier names for the same fault.
func checkAnchors(res *ExportResult, opt ExportVerifyOptions, events map[int64]anchorEvent, orgHashes map[int64][]byte) *Break {
	h := res.Header
	if h.Chain == ChainOrg {
		return checkOrgAnchors(res, opt, orgHashes)
	}
	entries := map[int64]ExportAnchor{}
	var brk *Break
	fail := func(seq int64, format string, args ...any) {
		if brk == nil || seq < brk.Seq {
			brk = &Break{Seq: seq, Reason: fmt.Sprintf(format, args...)}
		}
	}
	for _, a := range h.Anchors {
		if _, dup := entries[a.ChainSeq]; dup {
			fail(a.ChainSeq, "the header holds the anchor at chain_seq %d twice", a.ChainSeq)
			continue
		}
		entries[a.ChainSeq] = a
	}
	for seq, ev := range events {
		a, ok := entries[seq]
		if !ok {
			fail(seq, "the anchor event %s has no entry in the header: its anchored heads were removed from the file", ev.id)
			continue
		}
		res.Anchors++
		if a.EventID != ev.id {
			fail(seq, "the header names event %s for this anchor, the chain has %s", a.EventID, ev.id)
			continue
		}
		want, err := parseAnchorSummary(ev.summary)
		if err != nil {
			fail(seq, "%v", err)
			continue
		}
		if a.OrgCount != want.OrgCount || a.HeadsRoot != want.HeadsRoot {
			fail(seq, "the header's org_count and heads_root are not the ones the anchor event commits to")
			continue
		}
		if len(a.Heads) != want.OrgCount {
			fail(seq, "the anchor commits to %d org chains and the header holds %d heads for it", want.OrgCount, len(a.Heads))
			continue
		}
		heads, err := anchorHeads(a.Heads)
		if err != nil {
			fail(seq, "%v", err)
			continue
		}
		if hex.EncodeToString(HeadsRoot(h.InstallID, heads)) != want.HeadsRoot {
			fail(seq, "the anchored heads do not match the heads_root the anchor event commits to: a head was forged or removed")
		}
	}
	for seq := range entries {
		if _, ok := events[seq]; !ok {
			fail(seq, "the header names an anchor at chain_seq %d that is not an audit.anchor event of the chain: it was forged", seq)
		}
	}
	return brk
}

// anchorHeads decodes the heads of a header entry; it refuses a malformed or repeated head.
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
