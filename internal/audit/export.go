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
// decision 5, docs/site/src/content/docs/guides/audit-export.md). VerifyExport checks such a file
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
// TODO(ADR 0100 issue 6, anchors): the install export should carry each anchor's heads in the header;
// exportAnchors is where they come from once install_audit_anchor_heads exists.
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
	if err := exportAnchors(&hdr); err != nil {
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

// exportAnchors is the hook for issue 6 (anchors, ADR 0100 decision 4): once the anchor tables exist
// it adds each anchor's heads to the install export's header. Anchors do not exist yet, so it adds
// nothing, and VerifyExport has the matching hook (checkAnchors).
func exportAnchors(*ExportHeader) error { return nil }

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
	case ChainOrg:
		if h.OrgID == "" {
			return res, fmt.Errorf("%w: an org export has no org_id", ErrExportFormat)
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
		res.Break = checkAnchors(h, opt)
	}
	return res, nil
}

// checkAnchors is the hook for issue 6 (anchors) and issue 7 (checkpoints): it checks the anchor heads
// the header carries, and the signed checkpoint opt names, against the verified chain. Neither exists
// yet, so it checks nothing.
func checkAnchors(ExportHeader, ExportVerifyOptions) *Break { return nil }

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
	if r.Partial && !r.PrevChecked {
		_, _ = fmt.Fprintf(w, "note    the file starts at chain_seq %d: its first prev_hash is not checked (give the earlier export with --after)\n", r.Header.FromSeq)
	}
	return true
}
